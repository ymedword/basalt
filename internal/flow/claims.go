package flow

import (
	"errors"
	"fmt"
	"slices"
)

// ClaimRef 精确引用结论版本；旧验证不得自动套到新版本。
// ClaimRef identifies an exact claim version.
type ClaimRef struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

// ClaimRevision 保存可追溯结论、依赖与接受状态，不能由模型直接提升状态。
// ClaimRevision retains provenance and acceptance state controlled by verification.
type ClaimRevision struct {
	TaskID          string     `json:"task_id"`
	ID              string     `json:"claim_id"`
	Revision        int64      `json:"claim_revision"`
	TaskRevision    int64      `json:"task_revision"`
	Text            string     `json:"text"`
	State           string     `json:"state"`
	EvidenceIDs     []string   `json:"evidence_ids"`
	Dependencies    []ClaimRef `json:"dependencies"`
	VerificationIDs []string   `json:"verification_ids"`
	NodeAttemptID   string     `json:"-"`
}

// CurrentClaim binds an accepted conclusion to the actual verified producer
// output and attempt. Equal text from another attempt is not a reusable proof.
func CurrentClaim(revision TaskRevision, graph EvidenceGraph, id string) (ClaimRevision, bool) {
	output, ok := revision.Outputs[id]
	if !ok || !output.Verified || output.NodeAttemptID == "" {
		return ClaimRevision{}, false
	}
	var selected ClaimRevision
	for _, claim := range graph.Claims {
		if claim.ID == id && claim.State == "accepted" && claim.TaskRevision == output.Revision && output.Revision <= revision.Revision && claim.NodeAttemptID == output.NodeAttemptID && flowInputHash([]byte(claim.Text)) == output.Hash && claim.Revision > selected.Revision {
			selected = claim
		}
	}
	return selected, selected.Revision > 0
}

// ArtifactRef 精确引用产物版本。
// ArtifactRef identifies an immutable artifact version.
type ArtifactRef struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// ArtifactVersion 保存小型产物及依赖，验收通过与内容存在是两种事实。
// ArtifactVersion distinguishes stored content from accepted deliverables.
type ArtifactVersion struct {
	TaskID          string     `json:"task_id"`
	ID              string     `json:"artifact_id"`
	Version         int64      `json:"version"`
	TaskRevision    int64      `json:"task_revision"`
	Format          string     `json:"format"`
	Content         []byte     `json:"-"`
	ContentHash     string     `json:"content_hash"`
	State           string     `json:"state"`
	Claims          []ClaimRef `json:"claims"`
	VerificationIDs []string   `json:"verification_ids"`
}

// EvidenceGraph 是同任务依赖闭包的事务输入；来源正文不在其中复制。
// EvidenceGraph is a same-task dependency graph without duplicated source content.
type EvidenceGraph struct {
	TaskID    string
	Evidence  []EvidenceRef
	Claims    []ClaimRevision
	Artifacts []ArtifactVersion
}

func claimKey(ref ClaimRef) string { return fmt.Sprintf("%s@%d", ref.ID, ref.Revision) }

// ValidateGraph 在写关系之前整体验证归属、版本、限界和无环，拒绝部分失效。
// ValidateGraph checks ownership, exact versions, bounds and acyclicity before mutation.
func ValidateGraph(graph EvidenceGraph) error {
	if graph.TaskID == "" || len(graph.Evidence)+len(graph.Claims) > 1024 {
		return errors.New("flow evidence graph task or size invalid")
	}
	evidence := make(map[string]bool, len(graph.Evidence))
	for _, ref := range graph.Evidence {
		if ref.TaskID != graph.TaskID || ref.ID == "" || ref.SourceKind == "" || ref.SourceUID == "" || ref.SourceVersion == "" || ref.ContentHash == "" || ref.Location == "" || evidence[ref.ID] {
			return errors.New("flow evidence identity, owner or location invalid")
		}
		evidence[ref.ID] = true
	}
	claims := make(map[string]ClaimRevision, len(graph.Claims))
	edges := 0
	for _, claim := range graph.Claims {
		key := claimKey(ClaimRef{ID: claim.ID, Revision: claim.Revision})
		if claim.TaskID != graph.TaskID || !validID(claim.ID) || claim.Revision < 1 || claim.TaskRevision < 0 || !slices.Contains([]string{"proposed", "accepted", "stale", "superseded", "rejected"}, claim.State) {
			return errors.New("flow claim identity, owner or state invalid")
		}
		if _, found := claims[key]; found {
			return errors.New("flow claim version duplicated")
		}
		claims[key] = claim
		edges += len(claim.EvidenceIDs) + len(claim.Dependencies)
		if edges > 1024 {
			return errors.New("flow evidence relationship bound exceeded")
		}
		seen := make(map[string]bool)
		for _, id := range claim.EvidenceIDs {
			if !evidence[id] || seen[id] {
				return errors.New("flow claim evidence missing or duplicated")
			}
			seen[id] = true
		}
	}
	for _, claim := range graph.Claims {
		seen := make(map[string]bool)
		for _, dependency := range claim.Dependencies {
			key := claimKey(dependency)
			if _, found := claims[key]; !found || dependency.ID == claim.ID || seen[key] {
				return errors.New("flow claim dependency missing, duplicated or self-referential")
			}
			seen[key] = true
		}
	}
	colors := make(map[string]uint8)
	visits := 0
	var visit func(string) error
	visit = func(key string) error {
		if colors[key] == 1 {
			return errors.New("flow claim dependency cycle")
		}
		if colors[key] == 2 {
			return nil
		}
		colors[key] = 1
		for _, dependency := range claims[key].Dependencies {
			visits++
			if visits > 2048 {
				return errors.New("flow evidence traversal bound exceeded")
			}
			if err := visit(claimKey(dependency)); err != nil {
				return err
			}
		}
		colors[key] = 2
		return nil
	}
	for key := range claims {
		if err := visit(key); err != nil {
			return err
		}
	}
	totalBytes := 0
	seenArtifacts := make(map[ArtifactRef]bool)
	for _, artifact := range graph.Artifacts {
		key := ArtifactRef{ID: artifact.ID, Version: artifact.Version}
		if artifact.TaskID != graph.TaskID || !validID(artifact.ID) || artifact.Version < 1 || artifact.ContentHash == "" || seenArtifacts[key] || len(artifact.Content) > 1<<20 {
			return errors.New("flow artifact identity, owner or size invalid")
		}
		seenArtifacts[key] = true
		totalBytes += len(artifact.Content)
		if totalBytes > 8<<20 {
			return errors.New("flow artifact total size exceeded")
		}
		for _, dependency := range artifact.Claims {
			edges++
			if _, found := claims[claimKey(dependency)]; !found || edges > 1024 {
				return errors.New("flow artifact dependency missing or exceeds bound")
			}
		}
	}
	return nil
}

// InvalidationClosure 先计算完整失效集合，调用方再在一份短事务内接受。
// InvalidationClosure computes the entire closure before a short atomic commit.
func InvalidationClosure(graph EvidenceGraph, invalidEvidence []string) ([]ClaimRef, []ArtifactRef, error) {
	if err := ValidateGraph(graph); err != nil {
		return nil, nil, err
	}
	invalid := make(map[string]bool, len(invalidEvidence))
	for _, id := range invalidEvidence {
		found := false
		for _, ref := range graph.Evidence {
			found = found || ref.ID == id
		}
		if !found {
			return nil, nil, errors.New("flow invalidation source missing")
		}
		invalid[id] = true
	}
	// 建反向索引后逐边访问，不能反复全图扫描而越过2048边访问上限。
	// Reverse indexes keep closure traversal linear and within the edge budget.
	reverse := make(map[string][]ClaimRef)
	affected := make(map[string]bool)
	var refs []ClaimRef
	for _, claim := range graph.Claims {
		ref := ClaimRef{ID: claim.ID, Revision: claim.Revision}
		for _, dependency := range claim.Dependencies {
			key := claimKey(dependency)
			reverse[key] = append(reverse[key], ref)
		}
		for _, id := range claim.EvidenceIDs {
			if invalid[id] && !affected[claimKey(ref)] {
				affected[claimKey(ref)] = true
				refs = append(refs, ref)
			}
		}
	}
	for i := 0; i < len(refs); i++ {
		for _, dependent := range reverse[claimKey(refs[i])] {
			if !affected[claimKey(dependent)] {
				affected[claimKey(dependent)] = true
				refs = append(refs, dependent)
			}
		}
	}
	var artifacts []ArtifactRef
	for _, artifact := range graph.Artifacts {
		for _, ref := range artifact.Claims {
			if affected[claimKey(ref)] {
				artifacts = append(artifacts, ArtifactRef{ID: artifact.ID, Version: artifact.Version})
				break
			}
		}
	}
	return refs, artifacts, nil
}
