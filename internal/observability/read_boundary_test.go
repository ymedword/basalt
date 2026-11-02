package observability

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

// 事实端口的列边界是脱敏第一层：适配层不能把正文/身份/详情送入投影。
// Fact-port column boundaries prevent adapters from supplying bodies, identities, or details.
func rejectFactFields(t *testing.T, facts []any, forbidden ...string) {
	t.Helper()
	for _, fact := range facts {
		typ := reflect.TypeOf(fact)
		for _, name := range forbidden {
			_, present := typ.FieldByName(name)
			require.False(t, present, "%s must not carry %s across the read boundary", typ.Name(), name)
		}
	}
}

func TestObservability_NoMessageBody(t *testing.T) {
	rejectFactFields(t, []any{RunFacts{}, LLMCallFact{}, ToolInvocationFact{}, TaskExecutionFact{}, HistoryTrimView{}, CompressionView{}, CandidateView{}}, "Content", "Body", "FinalText", "ToolInput", "ToolResult", "Summary", "ReasoningContent", "ProviderExtra")
}

func TestObservability_OperationNoDetailJSON(t *testing.T) {
	rejectFactFields(t, []any{ManagementOperationFact{}}, "DetailJSON", "Detail", "ErrorMessage", "ResourceConfig", "TargetPath", "Credential")
}

func TestObservability_IngestNoDocumentTitle(t *testing.T) {
	rejectFactFields(t, []any{IngestJobFact{}, EmbeddingCallFact{}, CandidateView{}}, "Title", "DocumentTitle", "Content", "SourcePath", "ErrorMessage")
}
