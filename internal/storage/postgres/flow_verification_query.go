package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

func (s *FlowStore) ListVerifications(ctx context.Context, p core.Principal, id, cursor string, limit int) ([]flow.Verification, string, error) {
	if _, err := s.Read(ctx, p, id); err != nil {
		return nil, "", err
	}
	limit, err := flowPageLimit(limit)
	if err != nil {
		return nil, "", err
	}
	after, err := flowCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT detail FROM flow_verifications WHERE task_id=$1 AND verification_id>$2 ORDER BY verification_id LIMIT $3`, id, after, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]flow.Verification, 0, limit)
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, "", err
		}
		var v flow.Verification
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, "", err
		}
		if v.TaskID != id {
			return nil, "", flow.ErrStateConflict
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(items[len(items)-1].ID))
	}
	return items, next, nil
}

// ArtifactSources resolves the exact verification attempts, not a first page of
// history that could silently omit this artifact's source dependencies.
func (s *FlowStore) ArtifactSources(ctx context.Context, p core.Principal, id, artifactID string, version int64) ([]flow.EvidenceRef, bool, error) {
	if _, err := s.Read(ctx, p, id); err != nil {
		return nil, false, err
	}
	var witnessed bool
	err := s.db.pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_verifications v JOIN flow_node_attempts n ON n.task_id=v.task_id AND n.node_attempt_id=v.node_attempt_id LEFT JOIN runs r ON r.run_id=n.run_id AND r.flow_task_id=n.task_id AND r.flow_node_attempt_id=n.node_attempt_id WHERE v.task_id=$1 AND v.target_kind='artifact' AND v.target_id=$2 AND v.target_version=$3 AND n.state='succeeded' AND (n.run_id IS NULL OR r.status='completed'))`, id, artifactID, version).Scan(&witnessed)
	if err != nil || !witnessed {
		return nil, false, err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT DISTINCT e.evidence_id,e.source_kind,e.source_uid,e.source_version,e.location,e.content_hash,COALESCE(e.run_id,''),e.node_attempt_id FROM flow_verifications v JOIN flow_evidence_refs e ON e.task_id=v.task_id AND e.node_attempt_id=v.node_attempt_id WHERE v.task_id=$1 AND v.target_kind='artifact' AND v.target_id=$2 AND v.target_version=$3 ORDER BY e.evidence_id LIMIT 1025`, id, artifactID, version)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	refs := []flow.EvidenceRef{}
	for rows.Next() {
		ref := flow.EvidenceRef{TaskID: id}
		if err := rows.Scan(&ref.ID, &ref.SourceKind, &ref.SourceUID, &ref.SourceVersion, &ref.Location, &ref.ContentHash, &ref.RunID, &ref.NodeAttemptID); err != nil {
			return nil, false, err
		}
		refs = append(refs, ref)
	}
	if len(refs) > 1024 {
		return nil, false, flow.ErrCapacity
	}
	return refs, true, rows.Err()
}
