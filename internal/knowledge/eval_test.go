package knowledge

// 本文件交付 US-16 §4.5 的**离线**评测（T047）：固定数据集 + 固定查询集，
// embedding 用进程内确定性假件——不访问网络、不带 build tag。
//
// 三条口径必须先说清，否则读数会被误当成质量结论（C-19 / ADR-06）：
//  1. **它不是 ADR-06 的真实模型评测**。真实读数（`text-embedding-v4` /
//     `vector(512)` / D-05 冻结的 FullRecall@10）来自 `dev-env/evals/v2/data`
//     与真实调用，证据在 `dev-env/evidence/d05-embedding-eval.json`；本文件的指标
//     **只**覆盖 `testdata/knowledge/` 这套夹具，报告里 `quality_claim` 固定写
//     `none`，不得被引用成模型或产品的能力数字。
//  2. **两路检索都是词面假件**：假 embedding 是字符二元组哈希向量，关键词路是
//     字符三元组覆盖率。因此本文件判的是**管线回归**——范围解析 → 两路检索 →
//     RRF 融合 → top-k 截断 → 装配记录——不是语义质量。
//  3. **先冻结后测量**：夹具摘要与首轮读数都写进常量；改动夹具或管线必须显式
//     重新冻结，并在 `tasks.md` 附录留记录（ADR-06 冻结规则，不得事后挑选）。
//
// 标注主体：AI（2026-10-28 用户授权作为 US-16 ADR-06 指定评审人，见 D-05）；
// 每行标注都带 `label_status=ai_adjudicated_not_human`，**不得**写成人工标注。
//
// This file is the offline eval (T047): a fixed corpus plus a fixed query set with a
// deterministic in-process fake embedder. It guards the retrieval pipeline, never a
// model-quality claim: the report pins `quality_claim=none`, and the frozen numbers are
// first-run readings of this fixture rather than ADR-06 results (those live in D-05).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/require"

	"github.com/ymedword/basalt/internal/core"
)

// 评测夹具与冻结读数（2026-10-28 首轮测量后写入；改动必须重新冻结）。
// Fixture paths and the frozen first-run readings; any change must be re-frozen.
const (
	// evalRoot 是数据集根目录（相对包目录，`go test` 以包目录为工作目录）。
	evalRoot = "../../testdata/knowledge"
	// evalTopK 是质量指标的 k：ADR-06 的 nDCG@10 / MRR@10 与 top-k 截断都用它。
	evalTopK = 10
	// evalChunkMaxTokens / evalChunkOverlap 与生产默认值一致（knowledge.chunk.*），
	// 否则夹具片段与线上片段不同形，读数对不上产品行为。
	evalChunkMaxTokens = 400
	evalChunkOverlap   = 60
	// evalRunID 是装配记录的锚点；离线评测固定一个 Run，不伪造多个。
	evalRunID = "run_eval_us16"
	// evalEmbeddingName 明确标注假件：报告里出现 `fake:` 前缀即"不得当质量结论"。
	evalEmbeddingName = "fake:char-bigram-hash-v1"
	// evalQualityClaim 固定为 `none`（C-19：不得预填未测量的收益数字）。
	evalQualityClaim = "none"
	// evalDatasetName 是本夹具的名字，与 D-05 的 `adr06-us16-d05-v3` 区分开。
	evalDatasetName = "us16-offline-fixture"
	// evalLabelStatus 记录标注主体：AI 指定评审人，不是人工标注。
	evalLabelStatus = "ai_adjudicated_not_human"
)

// 冻结读数（首轮测量，2026-10-28）。门槛一律按"不得低于冻结值"判——允许管线
// 变好，不允许悄悄变差；变好到需要换基线时必须重新冻结并留记录。
// Frozen first-run readings; the gate is "never lower than this", never a loosened one.
const (
	// evalFrozenDatasetSHA256 是首轮（2026-10-28）对 5 篇语料 + 两份标注文件的
	// 规范摘要；夹具任何改动都会让它变，必须先重新冻结并留记录。
	evalFrozenDatasetSHA256 = "3b7578365def61fb6fb71d40a532e347999d5cdb3c143578d18d87115c298498"
	// evalFrozenRecallAtK 是"命中任一相关片段"的首轮读数（12 条可答查询全中）。
	evalFrozenRecallAtK = 1.0
	// evalFrozenFullRecallAtK 是"相关片段全中"的首轮读数（12/12）。
	evalFrozenFullRecallAtK = 1.0
	// evalFrozenNDCGAt10 是二元增益 nDCG@10 的首轮均值。
	evalFrozenNDCGAt10 = 0.883781487153574
	// evalFrozenMRRAt10 是首个相关片段名次倒数的首轮均值。
	evalFrozenMRRAt10 = 0.85
	// evalFrozenCitationSupport 是"top-k 证据逐字支撑全部答案要点"的首轮比率（12/12）。
	evalFrozenCitationSupport = 1.0
)

// evalKinds 是 ADR-06 的七类查询：夹具必须每类都有（设计 §2.6 / §4.5）。
// evalKinds is the seven ADR-06 query classes; every class must be present.
var evalKinds = []string{
	"zh_paraphrase",
	"identifier",
	"cross_section",
	"table",
	"conflict",
	"expired",
	"no_answer",
}

// evalQuery 是 `queries.jsonl` 的一行：查询本身与它的分类、可答性、标注说明。
// evalQuery is one line of queries.jsonl.
type evalQuery struct {
	// ID 是稳定标识，相关性标注按它关联。
	ID string `json:"id"`
	// Kind 是 ADR-06 七类之一。
	Kind string `json:"kind"`
	// Query 是送进检索的原始文本（不做改写、不加指令）。
	Query string `json:"query"`
	// Answerable 为假表示语料里没有答案（无答案类）。
	Answerable bool `json:"answerable"`
	// Rationale 说明这条查询要考什么，供复核者判断分类是否成立。
	Rationale string `json:"rationale"`
	// Source 记录标注主体与授权依据。
	Source string `json:"source"`
	// LabelStatus 区分 AI 判定与人工判定，**不得**混写。
	LabelStatus string `json:"label_status"`
}

// evalLabel 是 `relevance.jsonl` 的一行：相关片段与可接受答案要点。
// evalLabel is one line of relevance.jsonl.
type evalLabel struct {
	// QueryID 对应 `evalQuery.ID`。
	QueryID string `json:"query_id"`
	// ExpectedChunks 是相关片段（`文档#序号`，序号是切分器的 1 起 `Seq`）。
	// 冲突 / 过期类按**权威优先**排列，判定顺序性质时按此序解读。
	ExpectedChunks []string `json:"expected_chunks"`
	// AnswerPoints 是必须能在标注片段里逐字找到的答案要点（标注可追溯）。
	AnswerPoints []string `json:"answer_points"`
	// Confidence 是标注置信度；分歧与不可判样例保留，不改标注。
	Confidence string `json:"confidence"`
	// Source / LabelStatus 同 `evalQuery`。
	Source      string `json:"source"`
	LabelStatus string `json:"label_status"`
}

// evalDoc 是语料里的一份文档及其集合归属（按文件名前缀判定）。
// evalDoc is one corpus document with its collection.
type evalDoc struct {
	// Slug 是文件名去扩展名，也是片段标识的前缀。
	Slug string
	// Name 是文件名（进 `source_path`）。
	Name string
	// Collection 按前缀判定：`policy-` → policy，`incident-` → incident，其余 ops。
	Collection string
	// Text 是 UTF-8 正文。
	Text string
}

// evalChunk 是一片固定夹具：切分器产出 + 假向量。
// evalChunk is one fixture chunk: chunker output plus the fake vector.
type evalChunk struct {
	// ID 是 `文档#序号`，与 `relevance.jsonl` 的引用逐字一致。
	ID string
	// Doc 是文档 slug。
	Doc string
	// Seq 是切分器给出的 1 起序号。
	Seq int
	// Collection 是集合归属。
	Collection string
	// SourcePath 是文件名。
	SourcePath string
	// HeadingPath 是标题路径（进嵌入输入，与 D-05 规则一致）。
	HeadingPath string
	// Text 是片段正文，与原文逐字相同。
	Text string
	// ContentHash 是片段正文的 sha256（与 `kb_chunks.content_hash` 同口径）。
	ContentHash string
	// Vector 是假 embedding（已 L2 归一化）。
	Vector []float32
}

// hit 把夹具片段投影成检索端口返回的命中。
// hit projects the fixture chunk into a retrieval hit.
func (c evalChunk) hit() Hit {
	return Hit{
		ChunkID:      c.ID,
		DocumentID:   "doc_" + c.Doc,
		VersionID:    "ver_" + c.Doc + "_1",
		CollectionID: c.Collection,
		SourcePath:   c.SourcePath,
		HeadingPath:  c.HeadingPath,
		ContentHash:  c.ContentHash,
		Text:         c.Text,
	}
}

// evalDataset 是加载后的整套夹具：文档、片段、查询、标注与数据摘要。
// evalDataset is the loaded fixture: docs, chunks, queries, labels and digest.
type evalDataset struct {
	Docs    []evalDoc
	Chunks  []evalChunk
	Queries []evalQuery
	Labels  map[string]evalLabel
	Digest  string
}

// evalScore 是一条查询的质量读数（Recall any-hit / FullRecall / nDCG@10 / MRR@10）。
// evalScore is one query's quality readings.
type evalScore struct {
	// Recall 是"命中任一相关片段"（ADR-06 的 Recall@k）。
	Recall bool
	// FullRecall 是"相关片段全部命中"（跨段 / 冲突类最容易掉）。
	FullRecall bool
	// NDCG 是二元增益的 nDCG@10。
	NDCG float64
	// MRR 是首个相关片段名次的倒数（前 k 内没有相关记 0）。
	MRR float64
}

// evalReport 是离线评测报告：数据集摘要 + 指标 + 失败样例 + 口径声明。
// 它**不是** `docs/us/us16-eval-report.md`（收口产物），只是本用例的机器读数。
// evalReport is the machine-readable report; the closeout report is a separate file.
type evalReport struct {
	Dataset             string         `json:"dataset"`
	DatasetSHA256       string         `json:"dataset_sha256"`
	Embedding           string         `json:"embedding"`
	QualityClaim        string         `json:"quality_claim"`
	Chunker             string         `json:"chunker"`
	TopK                int            `json:"top_k"`
	Documents           int            `json:"documents"`
	Chunks              int            `json:"chunks"`
	Queries             int            `json:"queries"`
	AnswerableQueries   int            `json:"answerable_queries"`
	NoAnswerQueries     int            `json:"no_answer_queries"`
	ByKind              map[string]int `json:"by_kind"`
	RecallAtK           float64        `json:"recall_at_k"`
	FullRecallAtK       float64        `json:"full_recall_at_k"`
	NDCGAt10            float64        `json:"ndcg_at_10"`
	MRRAt10             float64        `json:"mrr_at_10"`
	CitationSupportRate float64        `json:"citation_support_rate"`
	AnswerableTop1Min   float64        `json:"answerable_top1_min_similarity"`
	NoAnswerTop1Max     float64        `json:"no_answer_top1_max_similarity"`
	NoAnswerTop1        []evalNoAnswer `json:"no_answer_top1"`
	FailureExamples     []string       `json:"failure_examples"`
	AuthorityRanks      []string       `json:"conflict_expired_ranks"`
}

// evalNoAnswer 是无答案查询的客观察数：只记录 top-1 相似度，**不设阈值**、
// 不判"判断正确率"（D-05 冻结规则 §2 的原话）。
// evalNoAnswer records the top-1 similarity only; no threshold is applied.
type evalNoAnswer struct {
	Query      string  `json:"query_id"`
	Top1       string  `json:"top1_chunk"`
	Similarity float64 `json:"top1_similarity"`
}

// TestEval_RetrievalQuality 是设计 §4.5 的离线检索质量评测：
// 数据集完整性（标注可追溯）→ 度量实现自证 → 固定夹具上的管线读数与冻结值比对。
//
// 逆改点（判别力，实测见 tasks.md 附录 R57）：
//   - `mergeRRF` 改成按分数升序 / 丢掉一路 → 召回与 nDCG 断言红；
//   - `fitBudget` 忽略 top-k → "命中数不得超过 k" 断言红；
//   - 标注指向不存在的片段或答案要点不在片段里 → 数据集断言红（夹具不能自说自话）。
//
// TestEval_RetrievalQuality is the offline retrieval-quality eval from design §4.5.
func TestEval_RetrievalQuality(t *testing.T) {
	dataset := loadEvalDataset(t)

	t.Run("dataset", func(t *testing.T) {
		validateEvalDataset(t, dataset)
		require.NotEmpty(t, evalFrozenDatasetSHA256, "数据集摘要必须先冻结再跑读数")
		require.NotEqual(t, "PENDING-FIRST-RUN", evalFrozenDatasetSHA256,
			"数据集摘要尚未冻结：先跑一轮取读数并写回常量，再提交")
		require.Equal(t, evalFrozenDatasetSHA256, dataset.Digest,
			"数据集已变更：ADR-06 冻结规则要求重新冻结并在 tasks.md 附录留记录")
	})

	t.Run("metrics", func(t *testing.T) {
		// 手工可算的玩具例：度量实现错了这里先红，避免"指标自证"变成管线回声。
		// Hand-computable toy cases pin the metric implementation itself.
		full := evalScoreQuery([]string{"x", "a", "y", "b"}, []string{"a", "b"}, evalTopK)
		require.True(t, full.Recall)
		require.True(t, full.FullRecall)
		require.Equal(t, 1.0/2.0, full.MRR)
		require.InDelta(t, 0.6509209298071326, full.NDCG, 1e-12)

		partial := evalScoreQuery([]string{"a", "x"}, []string{"a", "b"}, evalTopK)
		require.True(t, partial.Recall, "命中任一相关片段即 Recall")
		require.False(t, partial.FullRecall, "缺一个相关片段就不是 FullRecall")
		require.Equal(t, 1.0, partial.MRR)
		require.InDelta(t, 0.6131471927654584, partial.NDCG, 1e-12)

		third := evalScoreQuery([]string{"x", "y", "a"}, []string{"a"}, evalTopK)
		require.True(t, third.FullRecall)
		require.Equal(t, 1.0/3.0, third.MRR)
		require.InDelta(t, 0.5, third.NDCG, 1e-12, "名次 3 的二元增益是 1/log2(4)")

		missing := evalScoreQuery([]string{"x", "y"}, []string{"a"}, evalTopK)
		require.False(t, missing.Recall)
		require.False(t, missing.FullRecall)
		require.Zero(t, missing.MRR)
		require.Zero(t, missing.NDCG)

		// k 截断：第 11 名不算命中（nDCG@10 / MRR@10 只看前 10）。
		beyond := evalScoreQuery([]string{"x", "x", "x", "x", "x", "x", "x", "x", "x", "x", "a"}, []string{"a"}, evalTopK)
		require.False(t, beyond.Recall, "第 k+1 名不得算作命中")
		require.Zero(t, beyond.NDCG)
	})

	t.Run("pipeline", func(t *testing.T) {
		report := evalPipelineReport(t, dataset)
		encoded, err := json.MarshalIndent(report, "", "  ")
		require.NoError(t, err)
		t.Logf("离线评测报告（假 embedding，非质量结论）：\n%s", encoded)

		// C-19 口径：报告必须自曝"不是质量结论"，不得冒用 D-05 的真实读数。
		require.Equal(t, evalQualityClaim, report.QualityClaim, "离线夹具不得声称质量结论")
		require.Contains(t, report.Embedding, "fake:", "embedding 必须标明是假件")
		require.NotEmpty(t, report.DatasetSHA256)

		// 冻结门槛：允许变好，不允许悄悄变差（放宽即判据面变更）。
		require.GreaterOrEqual(t, report.RecallAtK, evalFrozenRecallAtK-1e-9, "Recall@k 低于冻结值")
		require.GreaterOrEqual(t, report.FullRecallAtK, evalFrozenFullRecallAtK-1e-9, "FullRecall@k 低于冻结值")
		require.GreaterOrEqual(t, report.NDCGAt10, evalFrozenNDCGAt10-1e-9, "nDCG@10 低于冻结值")
		require.GreaterOrEqual(t, report.MRRAt10, evalFrozenMRRAt10-1e-9, "MRR@10 低于冻结值")
		require.GreaterOrEqual(t, report.CitationSupportRate, evalFrozenCitationSupport-1e-9,
			"引用支持率低于冻结值")

		// 失败样例为空是本夹具的冻结结论——有失败就必须先解释，不能带着红读数提交。
		require.Empty(t, report.FailureExamples, "存在未召回的标注片段")

		// 冲突 / 过期类的**全部标注片段**都必须进 top-k（次序只记录、不设门槛）。
		require.Len(t, report.AuthorityRanks, 4,
			"冲突与过期各两条查询都要登记标注片段的名次")
	})
}

// loadEvalDataset 读取语料、查询与标注，并把每片正文投影成检索夹具。
// loadEvalDataset reads the corpus, queries and labels and projects every chunk.
func loadEvalDataset(t *testing.T) *evalDataset {
	t.Helper()
	root := filepath.FromSlash(evalRoot)
	entries, err := os.ReadDir(filepath.Join(root, "corpus"))
	require.NoError(t, err, "离线评测语料目录必须存在：%s/corpus", evalRoot)

	files := make([]evalFile, 0, len(entries)+2)
	dataset := &evalDataset{Labels: make(map[string]evalLabel)}
	chunker := NewTextChunker(evalChunkMaxTokens, evalChunkOverlap)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		name := entry.Name()
		raw, err := os.ReadFile(filepath.Join(root, "corpus", name))
		require.NoError(t, err)
		files = append(files, evalFile{rel: "corpus/" + name, raw: raw})

		slug := strings.TrimSuffix(name, ".md")
		doc := evalDoc{Slug: slug, Name: name, Collection: evalCollectionFor(slug), Text: string(raw)}
		require.NotEmpty(t, doc.Text, "语料 %s 不得为空", name)
		dataset.Docs = append(dataset.Docs, doc)

		drafts, err := chunker.Split(context.Background(), DocumentSource{Path: name, Text: doc.Text})
		require.NoError(t, err, "语料 %s 必须切得出片段", name)
		runes := []rune(doc.Text)
		for _, draft := range drafts {
			// 位置可追溯：片段正文必须是原文的逐字子串（来源视图按它取原文）。
			require.Equal(t, string(runes[draft.CharStart:draft.CharEnd]), draft.Text,
				"%s 的 char 区间必须能逐字回溯原文", name)
			chunk := evalChunk{
				ID:          fmt.Sprintf("%s#%d", slug, draft.Seq),
				Doc:         slug,
				Seq:         draft.Seq,
				Collection:  doc.Collection,
				SourcePath:  name,
				HeadingPath: draft.HeadingPath,
				Text:        draft.Text,
				ContentHash: draft.ContentHash,
			}
			// 嵌入输入与 D-05 规则一致："{heading_path}\n{text}"。
			chunk.Vector = evalEmbed(chunk.HeadingPath + "\n" + chunk.Text)
			dataset.Chunks = append(dataset.Chunks, chunk)
		}
	}

	for _, name := range []string{"queries.jsonl", "relevance.jsonl"} {
		raw, err := os.ReadFile(filepath.Join(root, name))
		require.NoError(t, err, "缺少 %s/%s", evalRoot, name)
		files = append(files, evalFile{rel: name, raw: raw})
	}

	dataset.Queries = decodeEvalJSONL[evalQuery](t, filepath.Join(root, "queries.jsonl"))
	for _, label := range decodeEvalJSONL[evalLabel](t, filepath.Join(root, "relevance.jsonl")) {
		_, dup := dataset.Labels[label.QueryID]
		require.Falsef(t, dup, "同一查询出现两行标注：%s", label.QueryID)
		dataset.Labels[label.QueryID] = label
	}
	dataset.Digest = evalDigest(t, files)
	return dataset
}

// decodeEvalJSONL 按 JSON 流逐行解码：`json.Decoder` 不会像默认 `bufio.Scanner`
// 那样在 64 KiB 处静默截断，长标注行也读得全。
// decodeEvalJSONL streams JSON values so long lines are never silently truncated.
func decodeEvalJSONL[T any](t *testing.T, path string) []T {
	t.Helper()
	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	decoder := json.NewDecoder(file)
	var out []T
	for {
		var row T
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoErrorf(t, err, "%s 必须是合法 JSONL", filepath.Base(path))
		out = append(out, row)
	}
	return out
}

// evalFile 是数据集里的一个原始文件：相对路径 + 原始字节（摘要输入）。
// evalFile is one raw dataset file: relative path plus bytes.
type evalFile struct {
	// rel 是相对数据集根目录的斜杠路径。
	rel string
	// raw 是文件原始字节，不做任何规范化。
	raw []byte
}

// evalDigest 按"路径 + 内容哈希"的规范字节算数据集摘要：路径必须排序，否则
// 目录读取顺序会影响读数（同一份数据两次跑出不同摘要）。
// evalDigest hashes sorted (path, content-hash) pairs so the digest is order-stable.
func evalDigest(t *testing.T, files []evalFile) string {
	t.Helper()
	slices.SortFunc(files, func(a, b evalFile) int {
		return strings.Compare(a.rel, b.rel)
	})
	hasher := sha256.New()
	for _, file := range files {
		sum := sha256.Sum256(file.raw)
		_, err := fmt.Fprintf(hasher, "%s\x00%s\n", file.rel, hex.EncodeToString(sum[:]))
		require.NoError(t, err)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// evalCollectionFor 按文件名前缀判定集合：语料是固定夹具，前缀规则写死在这里，
// 不读额外清单文件（清单越多，夹具越容易与断言脱节）。
// evalCollectionFor assigns the collection by filename prefix.
func evalCollectionFor(slug string) string {
	switch {
	case strings.HasPrefix(slug, "policy-"):
		return "policy"
	case strings.HasPrefix(slug, "incident-"):
		return "incident"
	default:
		return "ops"
	}
}

// validateEvalDataset 校验夹具自身：标注必须指向真实片段、答案要点必须逐字落在
// 标注片段里、七类查询齐全、无答案查询不得带标注。任一条不成立都是**夹具错误**，
// 不是检索错误——先修夹具，不能靠改断言让读数变好看。
// validateEvalDataset proves the fixture can be trusted before any metric is read.
func validateEvalDataset(t *testing.T, dataset *evalDataset) {
	t.Helper()
	byID := make(map[string]evalChunk, len(dataset.Chunks))
	for _, chunk := range dataset.Chunks {
		_, dup := byID[chunk.ID]
		require.Falsef(t, dup, "片段标识重复：%s", chunk.ID)
		byID[chunk.ID] = chunk
	}
	require.NotEmpty(t, byID, "语料必须切出片段")

	seenQueries := make(map[string]struct{}, len(dataset.Queries))
	byKind := make(map[string]int, len(evalKinds))
	for _, query := range dataset.Queries {
		require.NotEmpty(t, query.ID, "查询标识不得为空")
		require.NotEmpty(t, query.Query, "%s 的查询文本不得为空", query.ID)
		require.NotEmpty(t, query.Rationale, "%s 必须说明考什么", query.ID)
		_, dup := seenQueries[query.ID]
		require.Falsef(t, dup, "查询标识重复：%s", query.ID)
		seenQueries[query.ID] = struct{}{}
		byKind[query.Kind]++

		label, ok := dataset.Labels[query.ID]
		require.Truef(t, ok, "%s 缺少相关性标注", query.ID)
		require.Equal(t, evalLabelStatus, label.LabelStatus,
			"%s 的标注主体必须如实写成 AI 判定，不得冒充人工标注", query.ID)
		require.NotEmpty(t, label.Source, "%s 的标注必须记录来源", query.ID)
		require.NotEmpty(t, label.Confidence, "%s 的标注必须记录置信度", query.ID)
		require.Equalf(t, query.Answerable, len(label.ExpectedChunks) > 0,
			"%s 的可答性与标注不一致", query.ID)

		if query.Kind == "no_answer" {
			require.False(t, query.Answerable, "无答案类不得标成可答")
			require.Empty(t, label.AnswerPoints, "无答案查询不得有答案要点")
		}
		for _, ref := range label.ExpectedChunks {
			chunk, ok := byID[ref]
			require.Truef(t, ok, "%s 标注了不存在的片段 %s", query.ID, ref)
			require.Equal(t, fmt.Sprintf("%s#%d", chunk.Doc, chunk.Seq), ref,
				"片段标识必须与切分序号一致")
		}
		if !query.Answerable {
			continue
		}
		require.NotEmpty(t, label.AnswerPoints, "%s 必须给出可接受的答案要点", query.ID)
		for _, point := range label.AnswerPoints {
			grounded := false
			for _, ref := range label.ExpectedChunks {
				if strings.Contains(byID[ref].Text, point) {
					grounded = true
					break
				}
			}
			require.Truef(t, grounded, "%s 的答案要点 %q 在标注片段里找不到", query.ID, point)
		}
		for _, ref := range label.ExpectedChunks {
			carries := false
			for _, point := range label.AnswerPoints {
				if strings.Contains(byID[ref].Text, point) {
					carries = true
					break
				}
			}
			require.Truef(t, carries, "%s 的标注片段 %s 不承载任何答案要点", query.ID, ref)
		}
	}
	require.Len(t, dataset.Labels, len(dataset.Queries), "标注行数必须与查询数一致")
	for _, kind := range evalKinds {
		require.Positivef(t, byKind[kind], "ADR-06 七类查询缺一类：%s", kind)
	}
}

// evalScoreQuery 按 D-05 的度量定义打分：命中任一记 Recall，全部命中记
// FullRecall，nDCG 用二元增益、1/log2(i+1) 折损，MRR 取首个相关名次的倒数。
// 只有前 k 名参与计算——第 k+1 名不算命中。
// evalScoreQuery scores one query with the frozen D-05 metric definitions.
func evalScoreQuery(ids, expected []string, k int) evalScore {
	top := ids
	if len(top) > k {
		top = top[:k]
	}
	want := make(map[string]struct{}, len(expected))
	for _, id := range expected {
		want[id] = struct{}{}
	}
	seen := make(map[string]struct{}, len(want))
	dcg := 0.0
	score := evalScore{}
	for i, id := range top {
		if _, ok := want[id]; !ok {
			continue
		}
		seen[id] = struct{}{}
		dcg += 1 / math.Log2(float64(i)+2)
		if score.MRR == 0 {
			score.MRR = 1 / float64(i+1)
		}
	}
	score.Recall = len(seen) > 0
	score.FullRecall = len(seen) == len(want)
	idcg := 0.0
	for i := 0; i < len(want) && i < k; i++ {
		idcg += 1 / math.Log2(float64(i)+2)
	}
	if idcg > 0 {
		score.NDCG = dcg / idcg
	}
	return score
}

// evalSupports 判定检索到的正文能否逐字支撑全部答案要点（引用支持率的离线口径：
// 证据里找得到答案要点，才算"引用支持"）。
// evalSupports reports whether the retrieved text grounds every answer point.
func evalSupports(hits []Hit, points []string) bool {
	var evidence strings.Builder
	for _, hit := range hits {
		evidence.WriteString(hit.Text)
		evidence.WriteString("\n")
	}
	text := evidence.String()
	for _, point := range points {
		if !strings.Contains(text, point) {
			return false
		}
	}
	return true
}

// evalPipelineReport 跑完整管线并汇总报告：两次运行比对确定性、逐查询打分、
// 记录无答案查询的 top-1 相似度与失败样例。
// evalPipelineReport runs the pipeline twice and aggregates the report.
func evalPipelineReport(t *testing.T, dataset *evalDataset) evalReport {
	t.Helper()
	report := evalReport{
		Dataset:       evalDatasetName,
		DatasetSHA256: dataset.Digest,
		Embedding:     evalEmbeddingName,
		QualityClaim:  evalQualityClaim,
		Chunker:       fmt.Sprintf("v1:%d/%d", evalChunkMaxTokens, evalChunkOverlap),
		TopK:          evalTopK,
		Documents:     len(dataset.Docs),
		Chunks:        len(dataset.Chunks),
		Queries:       len(dataset.Queries),
		ByKind:        make(map[string]int, len(evalKinds)),
	}

	first, assembly := evalRunAll(t, dataset)
	require.Len(t, assembly.records, len(dataset.Queries), "每条查询都要写一条装配记录")
	for _, record := range assembly.records {
		require.Equal(t, evalRunID, record.RunID, "装配记录必须挂在固定 Run 上")
		require.Equal(t, core.PurposeRetrieval, record.Purpose, "装配记录用途必须是检索")
	}

	// 确定性：同一输入第二次运行必须逐字复现名次（map 遍历顺序不得泄漏进结果）。
	// Determinism: a second run must reproduce the exact ranking.
	second, _ := evalRunAll(t, dataset)
	for _, query := range dataset.Queries {
		require.Equalf(t, hitIDs(first[query.ID].Hits), hitIDs(second[query.ID].Hits),
			"%s 两次运行的命中次序必须一致", query.ID)
	}

	vectors := make(map[string]evalChunk, len(dataset.Chunks))
	for _, chunk := range dataset.Chunks {
		vectors[chunk.ID] = chunk
	}

	var (
		recall      float64
		full        float64
		ndcg        float64
		mrr         float64
		supported   int
		answerable  int
		top1Min     = math.Inf(1)
		noAnswerMax float64
	)
	for _, query := range dataset.Queries {
		report.ByKind[query.Kind]++
		label := dataset.Labels[query.ID]
		result := first[query.ID]
		top1, top1ID := evalTop1Similarity(query.Query, result.Hits, vectors)
		if query.Answerable {
			answerable++
			score := evalScoreQuery(hitIDs(result.Hits), label.ExpectedChunks, evalTopK)
			recall += evalBool(score.Recall)
			full += evalBool(score.FullRecall)
			ndcg += score.NDCG
			mrr += score.MRR
			if evalSupports(result.Hits, label.AnswerPoints) {
				supported++
			}
			if !score.FullRecall {
				report.FailureExamples = append(report.FailureExamples, fmt.Sprintf(
					"%s(%s) 未召回 %v；实取前 %d 名 %v",
					query.ID, query.Kind, evalMissing(hitIDs(result.Hits), label.ExpectedChunks),
					evalTopK, hitIDs(result.Hits)))
			}
			top1Min = math.Min(top1Min, top1)
			if ranks := evalRecordAuthority(t, query, label, hitIDs(result.Hits)); ranks != "" {
				report.AuthorityRanks = append(report.AuthorityRanks, ranks)
			}
			continue
		}
		report.NoAnswerQueries++
		report.NoAnswerTop1 = append(report.NoAnswerTop1, evalNoAnswer{
			Query: query.ID, Top1: top1ID, Similarity: top1,
		})
		noAnswerMax = math.Max(noAnswerMax, top1)
	}

	report.AnswerableQueries = answerable
	if answerable > 0 {
		report.RecallAtK = recall / float64(answerable)
		report.FullRecallAtK = full / float64(answerable)
		report.NDCGAt10 = ndcg / float64(answerable)
		report.MRRAt10 = mrr / float64(answerable)
		report.CitationSupportRate = float64(supported) / float64(answerable)
	}
	if !math.IsInf(top1Min, 1) {
		report.AnswerableTop1Min = top1Min
	}
	report.NoAnswerTop1Max = noAnswerMax
	// 无答案类**只记录** top-1 相似度，**不设阈值**（D-05 冻结规则 §2 的原话）：
	// 假向量总会给每个查询一个正余弦，词面共现也总能让覆盖率为正——把"可答最低
	// 相似度 > 无答案最高相似度"当门槛，等于在词面夹具上伪造一个语义阈值。
	// 无答案的终判在答案层（模型判无答案），检索层只负责把候选交出去。
	return report
}

// evalRunAll 用一条固定身份与固定参数跑完全部查询。
// evalRunAll runs every query with one fixed principal and one fixed parameter set.
func evalRunAll(t *testing.T, dataset *evalDataset) (map[string]RetrieveResult, *evalAssemblyRecorder) {
	t.Helper()
	retriever, assembly := newEvalRetriever(t, dataset)
	collections := evalCollections(dataset)
	out := make(map[string]RetrieveResult, len(dataset.Queries))
	for _, query := range dataset.Queries {
		result, err := retriever.Retrieve(context.Background(), RetrieveRequest{
			Principal:     evalPrincipal(),
			RunID:         evalRunID,
			StepID:        query.ID,
			CollectionIDs: collections,
			Query:         query.Query,
			Embedding:     evalEmbed(query.Query),
			Generation:    1,
			TopK:          evalTopK,
		})
		require.NoErrorf(t, err, "%s 检索失败", query.ID)
		require.Equalf(t, StrategyHybrid, result.Strategy, "%s 必须两路都可用", query.ID)
		require.Emptyf(t, result.DegradedReason, "%s 不得降级", query.ID)
		require.LessOrEqualf(t, len(result.Hits), evalTopK, "%s 的命中数不得超过 top-k", query.ID)
		out[query.ID] = result
	}
	return out, assembly
}

// newEvalRetriever 装配受测检索器：真 `Retriever` + 真 `mergeRRF` / `fitBudget`，
// 四个端口用进程内假件（本文件只判管线，不连数据库）。
// newEvalRetriever wires the real retriever over in-process fake ports.
func newEvalRetriever(t *testing.T, dataset *evalDataset) (*Retriever, *evalAssemblyRecorder) {
	t.Helper()
	logger, _ := captureLogger(t)
	assembly := &evalAssemblyRecorder{}
	retriever := NewRetriever(RetrieverDeps{
		Authz:       &evalAuthz{collections: evalCollections(dataset)},
		Keyword:     &evalKeywordPath{chunks: dataset.Chunks},
		Vectors:     &evalVectorPath{chunks: dataset.Chunks},
		Assembly:    assembly,
		Diagnostics: &evalDiagSink{},
		Config:      RetrievalConfig{TopK: evalTopK},
		Logger:      logger,
		Now:         fixedClock(),
	})
	return retriever, assembly
}

// evalCollections 返回夹具里出现过的集合（排序，保证请求与日志稳定）。
// evalCollections lists the fixture collections in stable order.
func evalCollections(dataset *evalDataset) []string {
	seen := make(map[string]struct{}, len(dataset.Docs))
	out := make([]string, 0, len(dataset.Docs))
	for _, doc := range dataset.Docs {
		if _, ok := seen[doc.Collection]; ok {
			continue
		}
		seen[doc.Collection] = struct{}{}
		out = append(out, doc.Collection)
	}
	slices.Sort(out)
	return out
}

// evalPrincipal 是持有 `knowledge:read` 的调用方身份（与生产准入同一形状）。
// evalPrincipal is a caller holding knowledge:read.
func evalPrincipal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: "caller_eval",
		KeyID:    "key_eval",
		Project:  "team-a",
		Scopes:   []string{core.ScopeKnowledgeRead},
		Actor:    core.ActorCaller,
	}
}

// evalRecordAuthority 记录冲突 / 过期类标注片段的实际名次：这两类必须**标注片段
// 全部召回**（ADR-06 的冲突 / 过期类判的就是"两段都读到才判得对"），但**不断言**
// 哪一段排前。
//
// 为什么不断言次序：权威优先是**答案层**的属性——制度片段自己写着"以本制度为准"，
// 模型两段都读到才判得对；而检索层的相对次序由词面信号决定，本文件的假 embedding /
// 三元组关键词路**没有**权威概念，按次序设阈值等于用夹具调参制造绿态。ADR-06 与
// 冻结度量（Recall / FullRecall / nDCG / MRR）也都没有这条判据。名次照录进报告，
// 变化可见但不构成门槛（tasks.md 附录 R57 记此裁决，含首轮实测次序）。
//
// evalRecordAuthority records the ranks of every annotated chunk for the conflict and
// expired classes. All of them must be retrieved; the relative order is *observed*, never
// gated — authority preference lives in the answer layer, and a lexical fixture has no
// notion of it.
func evalRecordAuthority(t *testing.T, query evalQuery, label evalLabel, ids []string) string {
	t.Helper()
	if query.Kind != "conflict" && query.Kind != "expired" {
		return ""
	}
	ranks := make([]string, 0, len(label.ExpectedChunks))
	for _, chunk := range label.ExpectedChunks {
		rank := evalRankOf(ids, chunk)
		require.Positivef(t, rank, "%s 的标注片段 %s 必须被检索到；实际次序 %v", query.ID, chunk, ids)
		ranks = append(ranks, fmt.Sprintf("%s@%d", chunk, rank))
	}
	return fmt.Sprintf("%s %s", query.ID, strings.Join(ranks, " "))
}

// evalRankOf 返回片段在结果里的名次（1 起），未出现返回 0。
// evalRankOf returns the 1-based rank, or zero when the chunk is absent.
func evalRankOf(ids []string, chunk string) int {
	for i, id := range ids {
		if id == chunk {
			return i + 1
		}
	}
	return 0
}

// evalMissing 列出标注里没有出现在前 k 名的片段。
// evalMissing lists the expected chunks absent from the returned ids.
func evalMissing(ids, expected []string) []string {
	present := make(map[string]struct{}, len(ids))
	for _, id := range ids[:min(len(ids), evalTopK)] {
		present[id] = struct{}{}
	}
	missing := make([]string, 0, len(expected))
	for _, id := range expected {
		if _, ok := present[id]; !ok {
			missing = append(missing, id)
		}
	}
	return missing
}

// evalBool 把布尔读数折成 0/1，便于求均值。
// evalBool folds a boolean reading into 0/1 for averaging.
func evalBool(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

// evalTop1Similarity 返回 top-1 命中的余弦相似度（无命中返回 0）：这是 D-05
// 规则里"无答案查询只记录 top-1 相似度、不设阈值"的那条读数。
// evalTop1Similarity returns the top-1 cosine similarity, or zero without hits.
func evalTop1Similarity(query string, hits []Hit, vectors map[string]evalChunk) (float64, string) {
	if len(hits) == 0 {
		return 0, ""
	}
	chunk, ok := vectors[hits[0].ChunkID]
	if !ok {
		return 0, hits[0].ChunkID
	}
	similarity := evalCosine(evalEmbed(query), chunk.Vector)
	if similarity < 0 {
		similarity = 0
	}
	return similarity, hits[0].ChunkID
}

// evalAuthz 解析可见范围：夹具里所有集合都对调用方可见（ACL 过滤由 R6 回归负责，
// 本文件判的是检索质量，不重复制造权限场景）。
// evalAuthz resolves every fixture collection as visible.
type evalAuthz struct {
	collections []string
}

// Resolve 按请求顺序返回可见集合；未请求时展开全部（与全库查询的测试口径一致）。
// Resolve returns the requested collections in order.
func (a *evalAuthz) Resolve(_ context.Context, _ core.Principal, _ string, collections []string) (Scope, error) {
	scope := Scope{}
	if len(collections) == 0 {
		scope.Collections = append(scope.Collections, a.collections...)
		return scope, nil
	}
	for _, id := range collections {
		if slices.Contains(a.collections, id) {
			scope.Collections = append(scope.Collections, id)
		}
	}
	return scope, nil
}

// evalKeywordPath 是关键词路径的假件：按字符三元组覆盖率排序，模拟 `pg_trgm`
// 的词面匹配。它**不**代替生产 SQL，只让 RRF 有第二路真实输入。
// evalKeywordPath ranks by rune-trigram coverage, mirroring pg_trgm's lexical matching.
type evalKeywordPath struct {
	chunks []evalChunk
}

// Search 返回覆盖率非零的片段：分数降序、同分按片段标识升序（确定性）。
// Search returns chunks with non-zero trigram coverage in a deterministic order.
func (p *evalKeywordPath) Search(_ context.Context, scope Scope, query string, k int) ([]Hit, int, error) {
	queryGrams := evalTrigrams(query)
	if len(queryGrams) == 0 {
		return nil, 0, nil
	}
	type ranked struct {
		hit   Hit
		score float64
	}
	scored := make([]ranked, 0, len(p.chunks))
	for _, chunk := range p.chunks {
		if !slices.Contains(scope.Collections, chunk.Collection) {
			continue
		}
		documentGrams := evalTrigrams(chunk.HeadingPath + "\n" + chunk.Text)
		shared := 0
		for gram := range queryGrams {
			if _, ok := documentGrams[gram]; ok {
				shared++
			}
		}
		if shared == 0 {
			continue
		}
		scored = append(scored, ranked{hit: chunk.hit(), score: float64(shared) / float64(len(queryGrams))})
	}
	slices.SortFunc(scored, func(a, b ranked) int {
		if order := compareFloatDesc(a.score, b.score); order != 0 {
			return order
		}
		return strings.Compare(a.hit.ChunkID, b.hit.ChunkID)
	})
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	hits := make([]Hit, 0, len(scored))
	for _, item := range scored {
		hits = append(hits, item.hit)
	}
	return hits, 0, nil
}

// evalVectorPath 是向量路径的假件：按假 embedding 的余弦相似度排序（>0 才算候选）。
// evalVectorPath ranks by cosine similarity over the fake embeddings.
type evalVectorPath struct {
	chunks []evalChunk
}

// Search 只查固定代际（本夹具只有一代），维度不符直接报错而不是补齐。
// Search reads the pinned generation only and never resizes a mismatched vector.
func (p *evalVectorPath) Search(_ context.Context, scope Scope, embedding []float32, generation, k int) ([]Hit, int, error) {
	if len(embedding) != VectorDimensions {
		return nil, 0, fmt.Errorf("查询向量维度 %d 与冻结维度 %d 不符", len(embedding), VectorDimensions)
	}
	if generation != 1 {
		return nil, 0, fmt.Errorf("夹具只有代际 1，收到 %d", generation)
	}
	type ranked struct {
		hit   Hit
		score float64
	}
	scored := make([]ranked, 0, len(p.chunks))
	for _, chunk := range p.chunks {
		if !slices.Contains(scope.Collections, chunk.Collection) {
			continue
		}
		similarity := evalCosine(embedding, chunk.Vector)
		if similarity <= 0 {
			continue
		}
		scored = append(scored, ranked{hit: chunk.hit(), score: similarity})
	}
	slices.SortFunc(scored, func(a, b ranked) int {
		if order := compareFloatDesc(a.score, b.score); order != 0 {
			return order
		}
		return strings.Compare(a.hit.ChunkID, b.hit.ChunkID)
	})
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	hits := make([]Hit, 0, len(scored))
	for _, item := range scored {
		hits = append(hits, item.hit)
	}
	return hits, 0, nil
}

// evalAssemblyRecorder 收集装配记录：本文件断言"每条查询一条记录、挂在固定 Run 上"。
// evalAssemblyRecorder collects assembly records for the per-query assertions.
type evalAssemblyRecorder struct {
	records []core.AssemblyRecord
}

// Record 记录一次装配（`Record` 无返回值，与生产端口一致）。
// Record appends one assembly record.
func (f *evalAssemblyRecorder) Record(_ context.Context, record core.AssemblyRecord) {
	f.records = append(f.records, record)
}

// evalDiagSink 丢弃诊断事件：离线评测不判诊断（那归检索侧用例与 OBS）。
// evalDiagSink discards diagnostic events; diagnostics are covered elsewhere.
type evalDiagSink struct{}

// Record 丢弃一条事件。
// Record drops one event.
func (f *evalDiagSink) Record(_ context.Context, _ DiagnosticEvent) {}

// evalEmbed 是确定性假 embedding：把字符二元组 / ASCII 词按 FNV-1a 哈希进
// `VectorDimensions` 个桶，再 L2 归一化（cosine 用归一化后的点积）。
// 同一文本永远得到同一向量——确定性是评测可复现的前提。
// evalEmbed hashes char bigrams into a fixed-width vector and L2-normalises it.
func evalEmbed(text string) []float32 {
	vector := make([]float32, VectorDimensions)
	hasher := fnv.New64a()
	for _, token := range evalTokens(text) {
		hasher.Reset()
		_, err := hasher.Write([]byte(token))
		if err != nil {
			continue
		}
		vector[hasher.Sum64()%uint64(VectorDimensions)]++
	}
	var norm float64
	for _, value := range vector {
		norm += float64(value) * float64(value)
	}
	if norm == 0 {
		return vector
	}
	norm = math.Sqrt(norm)
	for i := range vector {
		vector[i] = float32(float64(vector[i]) / norm)
	}
	return vector
}

// evalCosine 是两向量的余弦相似度：假向量已归一化，点积即可。
// evalCosine computes the dot product of two already-normalised vectors.
func evalCosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}

// evalTokens 把文本切成假 embedding 的输入单元：连续的汉字取二元组（单字取单字），
// 连续的 ASCII 字母 / 数字取一个小写词。中文没有空格，按字切是唯一不依赖分词的
// 确定性做法。
// evalTokens splits text into char bigrams for Han runs and lowercased ASCII words.
func evalTokens(text string) []string {
	runes := []rune(strings.ToLower(text))
	tokens := make([]string, 0, len(runes))
	for i := 0; i < len(runes); {
		switch {
		case unicode.Is(unicode.Han, runes[i]):
			end := i
			for end < len(runes) && unicode.Is(unicode.Han, runes[end]) {
				end++
			}
			run := runes[i:end]
			if len(run) == 1 {
				tokens = append(tokens, string(run))
			} else {
				for j := 0; j+1 < len(run); j++ {
					tokens = append(tokens, string(run[j:j+2]))
				}
			}
			i = end
		case isEvalASCII(runes[i]):
			end := i
			for end < len(runes) && isEvalASCII(runes[end]) {
				end++
			}
			tokens = append(tokens, string(runes[i:end]))
			i = end
		default:
			i++
		}
	}
	return tokens
}

// evalTrigrams 取"去掉空白后"的字符三元组：这是关键词路的匹配单元（近似 pg_trgm）。
// evalTrigrams builds rune trigrams over whitespace-trimmed text.
func evalTrigrams(text string) map[string]struct{} {
	compact := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, strings.ToLower(text))
	runes := []rune(compact)
	grams := make(map[string]struct{}, len(runes))
	if len(runes) < 3 {
		if len(runes) > 0 {
			grams[string(runes)] = struct{}{}
		}
		return grams
	}
	for i := 0; i+2 < len(runes); i++ {
		grams[string(runes[i:i+3])] = struct{}{}
	}
	return grams
}

// isEvalASCII 判定 ASCII 字母 / 数字（错误码与编号的主要字符集）。
// isEvalASCII reports ASCII letters and digits.
func isEvalASCII(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
}

// compareFloatDesc 按分数降序比较：NaN 视为最小（假件不该产生 NaN，兜底只为排序稳定）。
// compareFloatDesc orders scores descending and never panics on NaN.
func compareFloatDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	default:
		return 0
	}
}

// —— 记忆单因素对照（T060 / 设计卡 §4.5 记忆实验行）——
//
// 三臂逐字取自 §4.5：**无长期记忆 / 普通 Top-K / 受控记忆**，共用同一份夹具、
// 同一个假检索器与同一个 Token 预算——只变"记忆怎么控制"这一个因素。
//
// 三条口径与检索评测同源，必须先读：
//  1. **不是真实模型测量**：检索器是字符二元组哈希的假 embedding，读数只能证明
//     "控制策略在结构上确实不同"，**不得**当成任务成功率或产品收益；
//  2. **指标是夹具读数**：任务成功＝答案要点是否逐字出现在注入上下文里，不是
//     "模型答对了没有"；stale adoption / correction adherence / permission leakage
//     判的是**注入面**的准入，不是答案质量；
//  3. **未测量不得写收益**（C-19）：报告 `Benefit` 恒为 `未测量`，守卫拒绝任何
//     没有真实测量记录的收益结论。
//
// This is the T060 single-factor control: the three memory strategies from design
// §4.5 over one frozen fixture, one fake retriever and one token budget. It proves
// structural control, never a measured benefit.

const (
	// evalMemoryFile 是记忆对照的固定数据集（与检索夹具同目录、同一冻结纪律）。
	evalMemoryFile = "memory-control.json"
	// evalMemoryDatasetName 是报告自报的数据集名，与检索夹具区分开。
	evalMemoryDatasetName = "us16-memory-control"
	// evalMemoryRetriever 明确标注检索器是假件：三臂共用同一条排序实现。
	evalMemoryRetriever = "fake:char-bigram-hash-cosine-v1"
	// evalMemoryNotMeasured 是收益字段的唯一合法占位（C-19 / G1-25 同一字面量）。
	evalMemoryNotMeasured = "未测量"
	// 三臂名逐字取自设计卡 §4.5 的记忆实验行，**不得**改名。
	evalMemoryArmNoneName       = "无长期记忆"
	evalMemoryArmTopKName       = "普通 Top-K"
	evalMemoryArmControlledName = "受控记忆"
	// 三臂策略字面量：ArmReport 的机器判据（报告与守卫都按它判重）。
	evalMemoryStrategyNone       = "none"
	evalMemoryStrategyTopK       = "topk"
	evalMemoryStrategyControlled = "controlled"
	// evalMemoryPending 是"尚未冻结"的唯一字面量：没冻结过的报告不得当基线。
	evalMemoryPending = "PENDING-FIRST-RUN"
)

// 冻结读数（首轮测量后回填）：数据集摘要与三臂聚合读数先冻结、后比对；
// 读数变了必须显式改常量并说明原因，**不得**让"跑出来多少就算多少"滑过去。
const (
	evalMemoryFrozenDatasetSHA256 = "cb6d3144ee188704ab92598c01f9924d89de3491c2bf6e900469cabc84ea0f49"
	evalMemoryFrozenReadings      = "A:in=0,ok=0/3,stale=0,corr=0.00,leak=0,mark=0 | B:in=13,ok=3/3,stale=8,corr=0.00,leak=1,mark=4 | C:in=10,ok=3/3,stale=0,corr=1.00,leak=0,mark=0"
)

// evalMemoryNow 是夹具时钟：到期判定不读全局时间，读数才可复现。
// evalMemoryNow is the fixture clock; expiry never reads the wall clock.
var evalMemoryNow = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

// evalMemoryDataset 是记忆对照数据集的整体形态。
type evalMemoryDataset struct {
	Dataset      string             `json:"dataset"`
	Version      int                `json:"version"`
	LabelStatus  string             `json:"label_status"`
	Source       string             `json:"source"`
	BudgetTokens int                `json:"budget_tokens"`
	Principal    evalMemoryIdentity `json:"principal"`
	Entries      []evalMemoryEntry  `json:"entries"`
	Queries      []evalMemoryQuery  `json:"queries"`
	// Digest 是数据集文件的规范摘要（与检索夹具同口径）。
	Digest string `json:"-"`
}

// evalMemoryIdentity 是夹具里的调用方身份：字段与 core.Principal 可判字段一一对应。
type evalMemoryIdentity struct {
	CallerID string   `json:"caller_id"`
	KeyID    string   `json:"key_id"`
	Project  string   `json:"project"`
	Scopes   []string `json:"scopes"`
}

// principal 把夹具身份投影成生产同一形状的调用方身份。
func (p evalMemoryIdentity) principal() core.Principal {
	return core.Principal{
		Kind:     core.PrincipalCaller,
		CallerID: p.CallerID,
		KeyID:    p.KeyID,
		Project:  p.Project,
		Scopes:   p.Scopes,
		Actor:    core.ActorCaller,
	}
}

// evalMemoryEntry 是一条记忆条目：状态 / 有效期 / 分层都按记忆契约的字面量。
type evalMemoryEntry struct {
	ID           string `json:"id"`
	Scope        string `json:"scope"`
	Section      string `json:"section"`
	State        string `json:"state"`
	SupersedesID string `json:"supersedes_id"`
	ValidTo      string `json:"valid_to"`
	Content      string `json:"content"`
	Rationale    string `json:"rationale"`
}

// evalMemoryQuery 是一条对照查询：答案要点用于任务成功，marker 用于红线取证。
type evalMemoryQuery struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Query        string   `json:"query"`
	AnswerPoints []string `json:"answer_points"`
	StaleMarkers []string `json:"stale_markers"`
	LeakMarkers  []string `json:"leak_markers"`
	Rationale    string   `json:"rationale"`
}

// evalMemoryArm 是一条臂的配置声明 + 聚合读数。
// 配置字段（SameRetriever / SameBudget / BudgetTokens）是"单因素"的机器判据。
type evalMemoryArm struct {
	Arm           string
	Name          string
	Strategy      string
	SameRetriever bool
	SameBudget    bool
	BudgetTokens  int
	// Injected 是注入了多少条条目（跨全部查询累计）。
	Injected int
	// TaskSuccess / TaskTotal 只在有答案要点的查询上计。
	TaskSuccess int
	TaskTotal   int
	// StaleAdoption 是注入的非生效条目数（候选 / 被替代 / 纠正 / 删除 / 冲突 / 过期）。
	StaleAdoption int
	// CorrectionAdherence 是纠正类查询里"新事实在、旧事实不在"的比率。
	CorrectionAdherence float64
	// PermissionLeakage 是注入了调用方不可读分层的条目数（红线必须为 0）。
	PermissionLeakage int
	// MarkerHits 是上下文里出现旧事实 / 越权标记的次数（取证用，受控臂必须为 0）。
	MarkerHits int
}

// evalMemoryMeasurement 是一次**真实**测量的证据；没有它就不许写收益数字。
type evalMemoryMeasurement struct {
	Model      string // 真实模型版本（**不得**是 fake: / stub: 前缀）
	Runs       int    // 至少两轮才有可比性
	MeasuredAt string // YYYY-MM-DD
	Evidence   string // 读数落点（报告路径 / 段落）
}

// evalMemoryReport 是记忆对照的机器读数；不是收口产物 `us16-eval-report.md`。
type evalMemoryReport struct {
	Dataset        string
	DatasetSHA256  string
	Embedding      string // 检索用的假件标识
	Retriever      string
	BudgetTokens   int
	LabelStatus    string
	QualityClaim   string
	Benefit        string // 未测量时**必须**逐字写 `未测量`
	Measurement    *evalMemoryMeasurement
	Queries        int
	Arms           []evalMemoryArm
	FailureSamples []string
}

// TestEval_MemoryControl 是 §4.5 记忆实验的单因素对照：
// 数据集可复现 → 三臂配置只差一个因素 → 红线读数 → 冻结读数 → 收益守卫有判别力。
//
// 逆改点（判别力，实测见 tasks.md 附录 R107）：
//   - 受控臂去掉 state / valid_to 过滤 → `StaleAdoption` / `MarkerHits` 不再为 0，
//     "受控臂不得注入非生效条目" 断言红；
//   - 受控臂去掉分层 / 权限过滤 → `PermissionLeakage` 大于 0，红线断言红；
//   - 把 `Benefit` 改成任何收益数字 → `validate()` 红（无测量记录）；
//   - 给两臂设不同预算或不同检索器 → 单因素断言红。
func TestEval_MemoryControl(t *testing.T) {
	dataset := evalLoadMemoryControl(t)

	t.Run("dataset", func(t *testing.T) {
		evalValidateMemoryDataset(t, dataset)
		t.Logf("记忆对照数据集摘要：%s", dataset.Digest)
		require.NotEqualf(t, evalMemoryPending, evalMemoryFrozenDatasetSHA256,
			"数据集哈希尚未冻结：先跑一轮取读数并写回常量，再提交")
		require.Equalf(t, evalMemoryFrozenDatasetSHA256, dataset.Digest,
			"记忆对照数据集已变更：改夹具必须重新冻结哈希并重跑读数")
	})

	t.Run("arms", func(t *testing.T) {
		arms := evalMemoryArmsConfig(dataset.BudgetTokens)
		require.Len(t, arms, 3, "必须恰好三臂")
		require.Equal(t, evalMemoryArmNoneName, arms[0].Name, "A 臂名逐字取自 §4.5")
		require.Equal(t, evalMemoryArmTopKName, arms[1].Name, "B 臂名逐字取自 §4.5")
		require.Equal(t, evalMemoryArmControlledName, arms[2].Name, "C 臂名逐字取自 §4.5")

		seen := make(map[string]struct{}, len(arms))
		for _, arm := range arms {
			_, dup := seen[arm.Name]
			require.Falsef(t, dup, "%s 臂名与已有臂重复", arm.Arm)
			seen[arm.Name] = struct{}{}
			require.Truef(t, arm.SameRetriever, "%s 臂必须与对照共用同一检索器", arm.Arm)
			require.Truef(t, arm.SameBudget, "%s 臂必须与对照共用同一 Token 预算", arm.Arm)
			require.Equalf(t, dataset.BudgetTokens, arm.BudgetTokens, "%s 臂预算与数据集声明不符", arm.Arm)
		}
	})

	t.Run("run", func(t *testing.T) {
		report := evalMemoryReportFor(t, dataset)
		require.NoErrorf(t, report.validate(), "报告不合规：%s", evalMemoryDump(t, report))
		require.Equal(t, evalMemoryNotMeasured, report.Benefit, "未真实测量时收益只能写未测量")
		require.Nil(t, report.Measurement, "本轮没有真实测量记录")
		require.True(t, strings.HasPrefix(report.Embedding, "fake:"), "检索件必须标明是假件")
		require.NotEmptyf(t, report.FailureSamples, "三臂必然留下失败样例（A 臂全空、B 臂越权）")

		a, b, c := report.Arms[0], report.Arms[1], report.Arms[2]
		// A 臂：无记忆 = 不注入任何条目，任务全失。
		require.Zero(t, a.Injected, "无长期记忆臂不得注入任何条目")
		require.Zero(t, a.TaskSuccess, "无长期记忆臂不该有任何任务成功")
		// B 臂：普通 Top-K 会注入非生效 / 越权条目——这正是对照要暴露的。
		require.Positive(t, b.StaleAdoption, "普通 Top-K 臂必须暴露 stale adoption")
		require.Positive(t, b.PermissionLeakage, "普通 Top-K 臂必须暴露越权注入")
		require.Less(t, b.CorrectionAdherence, 1.0, "普通 Top-K 臂不该满足纠正一致性")
		// C 臂：受控记忆的四条红线。
		require.Zero(t, c.StaleAdoption, "受控臂不得注入非生效条目")
		require.Zero(t, c.MarkerHits, "受控臂上下文不得出现旧事实 / 越权标记")
		require.Zero(t, c.PermissionLeakage, "受控臂不得注入不可读记忆（permission leakage 红线）")
		require.Equal(t, 1.0, c.CorrectionAdherence, "受控臂必须满足纠正一致性")
		require.Equal(t, c.TaskTotal, c.TaskSuccess, "受控臂在有答案要点的查询上必须全中")

		got := evalMemoryReadingsKey(report.Arms)
		t.Logf("记忆三臂读数：%s", got)
		require.NotEqualf(t, evalMemoryPending, evalMemoryFrozenReadings, "冻结读数尚未回填")
		require.Equalf(t, evalMemoryFrozenReadings, got,
			"三臂读数与冻结值不一致——读数变了必须先确认原因再改冻结常量\n报告：%s",
			evalMemoryDump(t, report))

		// 可复现：同一夹具连跑两次，读数逐字相同。
		again := evalMemoryReportFor(t, dataset)
		require.Equalf(t, got, evalMemoryReadingsKey(again.Arms), "两次运行读数不一致：夹具必须确定性")
	})

	t.Run("guard", func(t *testing.T) {
		t.Run("未测量时合规", func(t *testing.T) {
			require.NoError(t, evalMemoryBaseline().validate())
		})

		t.Run("无测量记录的收益被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			err := r.validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "测量", "错误文案必须点明缺的是测量")
		})

		t.Run("假件模型测量不能证明收益", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			r.Measurement = &evalMemoryMeasurement{Model: evalMemoryRetriever, Runs: 3, MeasuredAt: "2026-10-29", Evidence: "x"}
			err := r.validate()
			require.Error(t, err)
			require.Contains(t, err.Error(), "假件", "错误文案必须点明假件 / 桩模型不含语义")
		})

		t.Run("轮次不足被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			r.Measurement = &evalMemoryMeasurement{Model: "qwen-max", Runs: 1, MeasuredAt: "2026-10-29", Evidence: "x"}
			require.ErrorContains(t, r.validate(), "少于两轮")
		})

		t.Run("缺测量时间被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			r.Measurement = &evalMemoryMeasurement{Model: "qwen-max", Runs: 2, Evidence: "x"}
			require.ErrorContains(t, r.validate(), "MeasuredAt")
		})

		t.Run("缺证据落点被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			r.Measurement = &evalMemoryMeasurement{Model: "qwen-max", Runs: 2, MeasuredAt: "2026-10-29"}
			require.ErrorContains(t, r.validate(), "Evidence")
		})

		t.Run("完整的真实测量放行", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Benefit = "任务成功率比无记忆臂高 12%"
			r.Measurement = &evalMemoryMeasurement{
				Model: "qwen-max", Runs: 2, MeasuredAt: "2026-10-29",
				Evidence: "docs/us/us16-eval-report.md#记忆实验",
			}
			require.NoError(t, r.validate(), "守卫不是一刀切拒绝：合规的真实测量必须放行")
		})

		t.Run("受控臂越权注入被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Arms[2].PermissionLeakage = 1
			require.ErrorContains(t, r.validate(), "红线")
		})

		t.Run("各臂预算不一致被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Arms[1].BudgetTokens = r.BudgetTokens + 30
			require.ErrorContains(t, r.validate(), "Token 预算")
		})

		t.Run("三臂配置重复被拒", func(t *testing.T) {
			r := evalMemoryBaseline()
			r.Arms[2].Strategy = r.Arms[1].Strategy
			require.ErrorContains(t, r.validate(), "三臂必须互不相同")
		})
	})
}

// evalLoadMemoryControl 读取记忆对照数据集并冻结其摘要。
func evalLoadMemoryControl(t *testing.T) *evalMemoryDataset {
	t.Helper()
	path := filepath.Join(filepath.FromSlash(evalRoot), evalMemoryFile)
	raw, err := os.ReadFile(path)
	require.NoErrorf(t, err, "缺少记忆对照数据集：%s/%s", evalRoot, evalMemoryFile)

	var dataset evalMemoryDataset
	require.NoErrorf(t, json.Unmarshal(raw, &dataset), "%s 必须是合法 JSON", evalMemoryFile)
	dataset.Digest = evalDigest(t, []evalFile{{rel: evalMemoryFile, raw: raw}})
	return &dataset
}

// evalValidateMemoryDataset 校验夹具自身：身份可用、条目状态 / 有效期可解析、
// 答案要点能在条目正文里逐字找到。夹具错了先修夹具，**不得**改断言。
func evalValidateMemoryDataset(t *testing.T, dataset *evalMemoryDataset) {
	t.Helper()
	require.Equal(t, evalMemoryDatasetName, dataset.Dataset, "数据集名与报告口径不符")
	require.Equal(t, 1, dataset.Version, "数据集版本")
	require.Equal(t, evalLabelStatus, dataset.LabelStatus, "标注来源必须如实标记（不是人工标注）")
	require.Positive(t, dataset.BudgetTokens, "Token 预算必须为正")
	require.True(t, dataset.Principal.principal().Valid(), "夹具身份必须是一份可用的调用方身份")
	require.True(t, dataset.Principal.principal().HasAgentScope(evalMemoryAgent(dataset)),
		"夹具身份必须持有它要读的那个 Agent 分层")

	byID := make(map[string]evalMemoryEntry, len(dataset.Entries))
	for _, entry := range dataset.Entries {
		require.NotEmptyf(t, entry.ID, "条目缺少 id")
		_, dup := byID[entry.ID]
		require.Falsef(t, dup, "条目 id 重复：%s", entry.ID)
		byID[entry.ID] = entry
		require.NotEmptyf(t, entry.Content, "%s 正文不得为空", entry.ID)
		require.Containsf(t, []string{string(core.SectionCore), string(core.SectionArchive)},
			entry.Section, "%s 的分区只能是 core / archive", entry.ID)
		require.Containsf(t, []string{"active", "candidate", "superseded", "corrected", "deleted", "conflicted"},
			entry.State, "%s 的状态必须是记忆契约闭集里的字面量", entry.ID)
		require.NotEmptyf(t, entry.Rationale, "%s 必须说明考什么", entry.ID)
		if entry.ValidTo != "" {
			_, err := time.Parse(time.RFC3339, entry.ValidTo)
			require.NoErrorf(t, err, "%s 的 valid_to 必须是 RFC3339", entry.ID)
		}
	}
	for _, entry := range dataset.Entries {
		if entry.SupersedesID == "" {
			continue
		}
		_, ok := byID[entry.SupersedesID]
		require.Truef(t, ok, "%s 的 supersedes_id 指向不存在的条目：%s", entry.ID, entry.SupersedesID)
	}

	wantKinds := []string{"core_freeze", "correction", "expiry", "candidate", "permission"}
	seenIDs := make(map[string]struct{}, len(dataset.Queries))
	seenKinds := make(map[string]int, len(wantKinds))
	for _, query := range dataset.Queries {
		require.NotEmptyf(t, query.ID, "查询缺少 id")
		_, dup := seenIDs[query.ID]
		require.Falsef(t, dup, "查询 id 重复：%s", query.ID)
		seenIDs[query.ID] = struct{}{}
		require.NotEmptyf(t, query.Query, "%s 的查询文本不得为空", query.ID)
		require.NotEmptyf(t, query.Rationale, "%s 必须说明考什么", query.ID)
		require.Containsf(t, wantKinds, query.Kind, "%s 未归类的查询类型", query.ID)
		seenKinds[query.Kind]++
		for _, point := range query.AnswerPoints {
			require.Truef(t, evalMemoryPointInEntries(dataset.Entries, point),
				"%s 的答案要点 %q 必须能在某条条目正文里逐字找到", query.ID, point)
		}
	}
	for _, kind := range wantKinds {
		require.Positivef(t, seenKinds[kind], "数据集缺少查询类型 %q", kind)
	}

	// 核心区冻结文本必须给召回留出预算余量，否则"同一 Token 预算"没有可比性。
	frozenTokens := evalMemoryTokens(evalMemoryText(evalMemoryFrozenCore(dataset)))
	require.LessOrEqualf(t, frozenTokens, dataset.BudgetTokens,
		"核心区冻结文本（%d token）超过对照预算（%d），召回没有余量", frozenTokens, dataset.BudgetTokens)
}

// evalMemoryPointInEntries 报告一个答案要点是否逐字落在某条条目正文里。
func evalMemoryPointInEntries(entries []evalMemoryEntry, point string) bool {
	for _, entry := range entries {
		if strings.Contains(entry.Content, point) {
			return true
		}
	}
	return false
}

// evalMemoryAgent 取夹具身份唯一的 Agent 分层名（`agents:invoke:<name>`）。
func evalMemoryAgent(dataset *evalMemoryDataset) string {
	for _, scope := range dataset.Principal.Scopes {
		if name, ok := strings.CutPrefix(scope, core.AgentInvokeScopePrefix); ok {
			return name
		}
	}
	return ""
}

// evalMemoryArmsConfig 返回三臂的**配置声明**：三组策略两两不同，其余因素逐字相同。
func evalMemoryArmsConfig(budget int) []evalMemoryArm {
	return []evalMemoryArm{
		{
			Arm: "A", Name: evalMemoryArmNoneName, Strategy: evalMemoryStrategyNone,
			SameRetriever: true, SameBudget: true, BudgetTokens: budget,
		},
		{
			Arm: "B", Name: evalMemoryArmTopKName, Strategy: evalMemoryStrategyTopK,
			SameRetriever: true, SameBudget: true, BudgetTokens: budget,
		},
		{
			Arm: "C", Name: evalMemoryArmControlledName, Strategy: evalMemoryStrategyControlled,
			SameRetriever: true, SameBudget: true, BudgetTokens: budget,
		},
	}
}

// evalMemoryBaseline 是守卫用例的合规底稿：字段齐全、三臂配置合法、收益未测量。
func evalMemoryBaseline() evalMemoryReport {
	budget := 60
	return evalMemoryReport{
		Dataset:       evalMemoryDatasetName,
		DatasetSHA256: strings.Repeat("0", 64),
		Embedding:     evalEmbeddingName,
		Retriever:     evalMemoryRetriever,
		BudgetTokens:  budget,
		LabelStatus:   evalLabelStatus,
		QualityClaim:  evalQualityClaim,
		Benefit:       evalMemoryNotMeasured,
		Queries:       5,
		Arms:          evalMemoryArmsConfig(budget),
	}
}

// validate 判卷报告是否合规：口径可追溯、三臂只差一个因素、受控臂不越权、
// **没有真实测量就不得出现收益数字**。返回 nil 表示合规。
func (r evalMemoryReport) validate() error {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch {
	case r.Dataset == "":
		fail("数据集名缺失")
	case r.DatasetSHA256 == "":
		fail("数据集哈希缺失")
	case !strings.HasPrefix(r.Embedding, "fake:"):
		fail("检索用 embedding 必须标明是假件（fake: 前缀），离线夹具不得冒用真实模型")
	case r.Retriever == "":
		fail("检索器口径缺失")
	case r.BudgetTokens <= 0:
		fail("Token 预算必须为正")
	case r.LabelStatus != evalLabelStatus:
		fail("标注来源必须如实标记（不是人工标注）")
	case r.QualityClaim != evalQualityClaim:
		fail("离线夹具不得声称质量结论")
	}

	if len(r.Arms) != 3 {
		fail("必须恰好三臂，实际 %d", len(r.Arms))
	}
	names := make(map[string]struct{}, len(r.Arms))
	strategies := make(map[string]struct{}, len(r.Arms))
	for _, arm := range r.Arms {
		if _, dup := names[arm.Name]; dup {
			fail("%s 臂名重复：%s", arm.Arm, arm.Name)
		}
		names[arm.Name] = struct{}{}
		if _, dup := strategies[arm.Strategy]; dup {
			fail("三臂必须互不相同：策略 %q 出现两次", arm.Strategy)
		}
		strategies[arm.Strategy] = struct{}{}
		if !arm.SameRetriever {
			fail("%s 臂没有共用对照的检索器——单因素被破坏", arm.Arm)
		}
		if !arm.SameBudget || arm.BudgetTokens != r.BudgetTokens {
			fail("%s 臂的 Token 预算与对照不一致（%d != %d）——单因素被破坏",
				arm.Arm, arm.BudgetTokens, r.BudgetTokens)
		}
		if arm.Strategy == evalMemoryStrategyControlled && arm.PermissionLeakage != 0 {
			fail("%s 臂越权注入 %d 条不可读记忆——permission leakage 红线必须为 0",
				arm.Arm, arm.PermissionLeakage)
		}
	}

	if r.Benefit != evalMemoryNotMeasured {
		m := r.Measurement
		switch {
		case m == nil:
			fail("收益 %q 没有测量记录：未测量只能写 %q", r.Benefit, evalMemoryNotMeasured)
		case strings.HasPrefix(m.Model, "fake:") || strings.HasPrefix(m.Model, "stub:"):
			fail("收益 %q 的测量模型是假件 / 桩（%s），不含语义，不能证明收益", r.Benefit, m.Model)
		case m.Runs < 2:
			fail("测量少于两轮（%d），没有可比性", m.Runs)
		case m.MeasuredAt == "":
			fail("测量记录缺 MeasuredAt（测量时间）")
		case m.Evidence == "":
			fail("测量记录缺 Evidence（读数落点）")
		}
	}

	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "；"))
	}
	return nil
}

// evalMemoryReportFor 在三臂上跑同一份夹具，产出结构性报告。
func evalMemoryReportFor(t *testing.T, dataset *evalMemoryDataset) evalMemoryReport {
	t.Helper()
	report := evalMemoryReport{
		Dataset:       dataset.Dataset,
		DatasetSHA256: dataset.Digest,
		Embedding:     evalEmbeddingName,
		Retriever:     evalMemoryRetriever,
		BudgetTokens:  dataset.BudgetTokens,
		LabelStatus:   dataset.LabelStatus,
		QualityClaim:  evalQualityClaim,
		// C-19：夹具只给结构读数，收益恒写"未测量"。
		Benefit: evalMemoryNotMeasured,
		Queries: len(dataset.Queries),
		Arms:    evalMemoryArmsConfig(dataset.BudgetTokens),
	}
	principal := dataset.Principal.principal()
	for i := range report.Arms {
		arm, samples := evalMemoryScoreArm(t, dataset, report.Arms[i], principal)
		report.Arms[i] = arm
		report.FailureSamples = append(report.FailureSamples, samples...)
	}
	return report
}

// evalMemoryScoreArm 在全部查询上跑一条臂并聚合四项指标。
func evalMemoryScoreArm(t *testing.T, dataset *evalMemoryDataset, arm evalMemoryArm,
	principal core.Principal,
) (evalMemoryArm, []string) {
	t.Helper()
	var samples []string
	corrections, correctionsOK := 0, 0

	for _, query := range dataset.Queries {
		entries := evalMemoryContext(t, dataset, arm.Strategy, query)
		text := evalMemoryText(entries)
		arm.Injected += len(entries)

		for _, entry := range entries {
			if !evalMemoryAuthoritative(entry) {
				arm.StaleAdoption++
				samples = append(samples, fmt.Sprintf("%s/%s 注入非生效条目 %s（state=%s, valid_to=%q）",
					arm.Arm, query.ID, entry.ID, entry.State, entry.ValidTo))
			}
			if !evalMemoryReadable(entry, principal) {
				arm.PermissionLeakage++
				samples = append(samples, fmt.Sprintf("%s/%s 注入不可读条目 %s（scope=%q）",
					arm.Arm, query.ID, entry.ID, entry.Scope))
			}
		}

		for _, marker := range query.StaleMarkers {
			if strings.Contains(text, marker) {
				arm.MarkerHits++
				samples = append(samples, fmt.Sprintf("%s/%s 上下文含旧事实标记 %q", arm.Arm, query.ID, marker))
			}
		}
		for _, marker := range query.LeakMarkers {
			if strings.Contains(text, marker) {
				arm.MarkerHits++
				samples = append(samples, fmt.Sprintf("%s/%s 上下文含越权标记 %q", arm.Arm, query.ID, marker))
			}
		}

		missing := evalMemoryMissing(text, query.AnswerPoints)
		if len(query.AnswerPoints) > 0 {
			arm.TaskTotal++
			if len(missing) == 0 {
				arm.TaskSuccess++
			} else {
				samples = append(samples, fmt.Sprintf("%s/%s 缺答案要点 %v", arm.Arm, query.ID, missing))
			}
		}
		if query.Kind == "correction" {
			corrections++
			if len(missing) == 0 && !evalMemoryAnyMarker(text, query.StaleMarkers) {
				correctionsOK++
			}
		}
	}
	if corrections > 0 {
		arm.CorrectionAdherence = float64(correctionsOK) / float64(corrections)
	}
	return arm, samples
}

// evalMemoryContext 返回一条臂在一条查询下实际注入的条目。
// 三臂的差别**只有**记忆控制策略：检索器、预算与查询集逐字相同。
func evalMemoryContext(t *testing.T, dataset *evalMemoryDataset, strategy string, query evalMemoryQuery) []evalMemoryEntry {
	t.Helper()
	switch strategy {
	case evalMemoryStrategyNone:
		return nil
	case evalMemoryStrategyTopK:
		// 普通 Top-K：全部条目一起按相似度排，不看状态、分层或有效期。
		return evalMemoryFill(evalMemoryRank(query.Query, dataset.Entries), dataset.BudgetTokens, 0)
	case evalMemoryStrategyControlled:
		// 受控记忆：核心区冻结文本整段注入（永不裁剪），归档区按受控视图召回。
		frozen := evalMemoryFrozenCore(dataset)
		used := evalMemoryTokens(evalMemoryText(frozen))
		recalled := evalMemoryFill(
			evalMemoryRank(query.Query, evalMemoryControlledCandidates(dataset)),
			dataset.BudgetTokens, used)
		return append(append([]evalMemoryEntry{}, frozen...), recalled...)
	default:
		require.FailNowf(t, "未知记忆对照臂", "strategy=%q", strategy)
		return nil
	}
}

// evalMemoryFrozenCore 按 memory.LoadEffective 的口径取生效层核心区：
// Agent 层任一分区有正文 → 该层；否则回退全局层。冻结文本只含核心区。
func evalMemoryFrozenCore(dataset *evalMemoryDataset) []evalMemoryEntry {
	layer := evalMemoryLayer(dataset, evalMemoryAgent(dataset))
	if len(layer) == 0 {
		layer = evalMemoryLayer(dataset, "")
	}
	out := make([]evalMemoryEntry, 0, len(layer))
	for _, entry := range layer {
		if entry.Section == string(core.SectionCore) {
			out = append(out, entry)
		}
	}
	return out
}

// evalMemoryLayer 返回某一分层的全部条目（任意分区）。
func evalMemoryLayer(dataset *evalMemoryDataset, agent string) []evalMemoryEntry {
	out := make([]evalMemoryEntry, 0, len(dataset.Entries))
	for _, entry := range dataset.Entries {
		if entry.Scope == agent {
			out = append(out, entry)
		}
	}
	return out
}

// evalMemoryControlledCandidates 返回受控视图的归档区候选：
// 生效（active）且未过期、属于本 Run 的 Agent 分层、调用方有权读取。
// 核心区已由冻结面覆盖，不重复召回。
func evalMemoryControlledCandidates(dataset *evalMemoryDataset) []evalMemoryEntry {
	agent := evalMemoryAgent(dataset)
	principal := dataset.Principal.principal()
	out := make([]evalMemoryEntry, 0, len(dataset.Entries))
	for _, entry := range dataset.Entries {
		if entry.Scope != agent || entry.Section != string(core.SectionArchive) {
			continue
		}
		if entry.State != "active" || evalMemoryExpiredAt(entry, evalMemoryNow) {
			continue
		}
		if !evalMemoryReadable(entry, principal) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// evalMemoryAuthoritative 判条目是否生效：active 且未过 valid_to。
func evalMemoryAuthoritative(entry evalMemoryEntry) bool {
	return entry.State == "active" && !evalMemoryExpiredAt(entry, evalMemoryNow)
}

// evalMemoryExpiredAt 判条目在给定时点是否已到期；解析失败按到期处理（fail-closed）。
func evalMemoryExpiredAt(entry evalMemoryEntry, now time.Time) bool {
	if entry.ValidTo == "" {
		return false
	}
	validTo, err := time.Parse(time.RFC3339, entry.ValidTo)
	if err != nil {
		return true
	}
	return !validTo.After(now)
}

// evalMemoryReadable 判调用方能否读取该条目所在分层：全局层人人可读，
// Agent 层要求持有对应的 `agents:invoke:<name>`。
func evalMemoryReadable(entry evalMemoryEntry, principal core.Principal) bool {
	if entry.Scope == "" {
		return true
	}
	return principal.HasAgentScope(entry.Scope)
}

// evalMemoryRank 按假 embedding 的余弦相似度排序：三臂共用这一条检索器，
// 同分按条目 id 稳定破平（map 顺序不得泄漏进结果）。
func evalMemoryRank(query string, entries []evalMemoryEntry) []evalMemoryEntry {
	type scored struct {
		entry evalMemoryEntry
		score float64
	}
	queryVector := evalEmbed(query)
	ranked := make([]scored, 0, len(entries))
	for _, entry := range entries {
		ranked = append(ranked, scored{entry: entry, score: evalCosine(queryVector, evalEmbed(entry.Content))})
	}
	slices.SortStableFunc(ranked, func(a, b scored) int {
		if cmp := compareFloatDesc(a.score, b.score); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.entry.ID, b.entry.ID)
	})
	out := make([]evalMemoryEntry, 0, len(ranked))
	for _, item := range ranked {
		out = append(out, item.entry)
	}
	return out
}

// evalMemoryFill 按名次贪心填满预算：装不下的条目跳过，继续看后面的（确定性）。
func evalMemoryFill(ranked []evalMemoryEntry, budget, used int) []evalMemoryEntry {
	out := make([]evalMemoryEntry, 0, len(ranked))
	for _, entry := range ranked {
		cost := evalMemoryTokens(entry.Content)
		if used+cost > budget {
			continue
		}
		used += cost
		out = append(out, entry)
	}
	return out
}

// evalMemoryText 把注入条目拼成上下文（逐字保留正文，不加修饰）。
func evalMemoryText(entries []evalMemoryEntry) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		parts = append(parts, entry.Content)
	}
	return strings.Join(parts, "\n")
}

// evalMemoryTokens 是夹具内的确定性 Token 估算：非空白字符数（中文一字≈一 token）。
// 它是结构对照的公共度量，**不得**当成真实模型的计费读数。
func evalMemoryTokens(text string) int {
	count := 0
	for _, r := range text {
		if !unicode.IsSpace(r) {
			count++
		}
	}
	return count
}

// evalMemoryMissing 返回上下文里缺失的答案要点。
func evalMemoryMissing(text string, points []string) []string {
	var missing []string
	for _, point := range points {
		if !strings.Contains(text, point) {
			missing = append(missing, point)
		}
	}
	return missing
}

// evalMemoryAnyMarker 报告上下文是否含任一标记。
func evalMemoryAnyMarker(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// evalMemoryReadingsKey 把三臂读数压成一行冻结键（与压缩三臂同一纪律）。
func evalMemoryReadingsKey(arms []evalMemoryArm) string {
	parts := make([]string, 0, len(arms))
	for _, arm := range arms {
		parts = append(parts, fmt.Sprintf("%s:in=%d,ok=%d/%d,stale=%d,corr=%.2f,leak=%d,mark=%d",
			arm.Arm, arm.Injected, arm.TaskSuccess, arm.TaskTotal,
			arm.StaleAdoption, arm.CorrectionAdherence, arm.PermissionLeakage, arm.MarkerHits))
	}
	return strings.Join(parts, " | ")
}

// evalMemoryDump 把报告编码成失败信息里的可读载荷。
func evalMemoryDump(t *testing.T, report evalMemoryReport) string {
	t.Helper()
	encoded, err := json.MarshalIndent(report, "", "  ")
	require.NoError(t, err)
	return string(encoded)
}
