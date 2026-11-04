package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ymedword/basalt/internal/core"
	"github.com/ymedword/basalt/internal/flow"
)

// FlowStore is the sole PostgreSQL authority for durable Flow mutations. Calls
// happen outside its short transactions; every state mutation locks the root.
type FlowStore struct{ db *DB }

// NewFlowStore binds an existing instance database without starting workers.
func NewFlowStore(db *DB) *FlowStore { return &FlowStore{db: db} }

var _ flow.DefinitionRepository = (*FlowStore)(nil)

const flowDefinitionColumns = `d.definition_uid,d.version,d.content_hash,d.definition_content,d.contract_content,d.created_at,p.retired`

type flowScanner interface{ Scan(...any) error }

func scanFlowDefinition(row flowScanner) (flow.DefinitionVersion, error) {
	var result flow.DefinitionVersion
	var content, contract []byte
	if err := row.Scan(&result.DefinitionUID, &result.Version, &result.ContentHash, &content, &contract, &result.PublishedAt, &result.Retired); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, flow.ErrDefinitionNotFound
		}
		return result, err
	}
	definition, err := flow.DecodeDefinitionJSON(context.Background(), content)
	if err != nil {
		return result, err
	}
	definition.Contract, err = flow.DecodeTaskContractJSON(context.Background(), contract)
	if err != nil {
		return result, err
	}
	hash, err := flow.DefinitionHash(definition)
	if err != nil || hash != result.ContentHash {
		return result, flow.ErrDefinitionInvalid
	}
	result.Definition = definition
	return result, nil
}

func (s *FlowStore) CurrentPointer(ctx context.Context, uid string) (flow.DefinitionVersion, error) {
	return readFlowCurrent(ctx, s.db.pool, uid)
}

func readFlowCurrent(ctx context.Context, q Queryer, uid string) (flow.DefinitionVersion, error) {
	return scanFlowDefinition(q.QueryRowContext(ctx, `SELECT `+flowDefinitionColumns+` FROM flow_pointers p JOIN flow_definitions d ON d.definition_uid=p.definition_uid AND d.version=p.current_version WHERE p.definition_uid=$1`, uid))
}

func (s *FlowStore) FindDefinition(ctx context.Context, name string) (flow.DefinitionVersion, error) {
	return scanFlowDefinition(s.db.pool.QueryRowContext(ctx, `SELECT `+flowDefinitionColumns+` FROM flow_pointers p JOIN flow_definitions d ON d.definition_uid=p.definition_uid AND d.version=p.current_version WHERE lower(p.name)=lower($1)`, name))
}

func (s *FlowStore) ResolveCurrent(ctx context.Context, uid string, requestedVersion int64) (flow.DefinitionVersion, error) {
	if requestedVersion < 0 {
		return flow.DefinitionVersion{}, flow.ErrDefinitionInvalid
	}
	current, err := s.CurrentPointer(ctx, uid)
	if err != nil {
		return current, err
	}
	if current.Retired {
		return current, flow.ErrDefinitionRetired
	}
	if requestedVersion > 0 && requestedVersion != current.Version {
		var exists bool
		if err := s.db.pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM flow_definitions WHERE definition_uid=$1 AND version=$2)`, uid, requestedVersion).Scan(&exists); err != nil {
			return current, err
		}
		if !exists {
			return current, flow.ErrDefinitionVersionNotFound
		}
		return current, flow.ErrDefinitionNotCurrent
	}
	return current, nil
}

func (s *FlowStore) ReadDefinition(ctx context.Context, ref flow.DefinitionRef) (flow.DefinitionVersion, error) {
	result, err := scanFlowDefinition(s.db.pool.QueryRowContext(ctx, `SELECT `+flowDefinitionColumns+` FROM flow_definitions d JOIN flow_pointers p ON p.definition_uid=d.definition_uid WHERE d.definition_uid=$1 AND d.version=$2`, ref.UID, ref.Version))
	if errors.Is(err, flow.ErrDefinitionNotFound) {
		return result, flow.ErrDefinitionVersionNotFound
	}
	return result, err
}

func (s *FlowStore) ListDefinitions(ctx context.Context, includeRetired bool, cursor string, limit int) ([]flow.DefinitionVersion, string, error) {
	limit, err := flowPageLimit(limit)
	if err != nil {
		return nil, "", err
	}
	rows, err := s.db.pool.QueryContext(ctx, `SELECT `+flowDefinitionColumns+` FROM flow_pointers p JOIN flow_definitions d ON d.definition_uid=p.definition_uid AND d.version=p.current_version WHERE ($1 OR NOT p.retired) AND p.definition_uid>$2 ORDER BY p.definition_uid LIMIT $3`, includeRetired, cursor, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	result := make([]flow.DefinitionVersion, 0, limit)
	for rows.Next() {
		item, err := scanFlowDefinition(rows)
		if err != nil {
			return nil, "", err
		}
		if len(result) == limit {
			return result, result[len(result)-1].DefinitionUID, nil
		}
		result = append(result, item)
	}
	return result, "", rows.Err()
}

func (s *FlowStore) Publish(ctx context.Context, req flow.PublishRequest) (flow.DefinitionVersion, error) {
	var result flow.DefinitionVersion
	if !req.Principal.Valid() || req.Principal.Kind != core.PrincipalLocalAdmin || req.OperationID == "" {
		return result, core.ErrUnauthorized
	}
	definition, contract, err := flow.DefinitionContent(req.Definition)
	if err != nil {
		return result, err
	}
	hash, err := flow.DefinitionHash(req.Definition)
	if err != nil {
		return result, err
	}
	err = s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return flowOperationCommitted(ctx, q, req.OperationID)
	}, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockFlowDefinitions(ctx, tx); err != nil {
			return err
		}
		current, err := readFlowCurrent(ctx, tx, req.DefinitionUID)
		if err != nil && !errors.Is(err, flow.ErrDefinitionNotFound) {
			return err
		}
		if current.Version != req.ExpectedVersion {
			return core.ErrPreconditionFailed
		}
		version := current.Version + 1
		if current.Version > 0 && !current.Retired && current.ContentHash == hash {
			version = current.Version
		}
		if err := checkFlowPendingOperation(ctx, tx, req.OperationID, "flow.publish", req.DefinitionUID, version); err != nil {
			return err
		}
		now := s.db.clock()
		if version != current.Version {
			_, err = tx.ExecContext(ctx, `INSERT INTO flow_definitions(definition_uid,version,name,content_hash,definition_content,contract_content,created_at,operation_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, req.DefinitionUID, version, req.Definition.Name, hash, string(definition), string(contract), now.UnixMilli(), req.OperationID)
			if err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO flow_pointers(definition_uid,name,current_version,retired,updated_at,operation_id) VALUES($1,$2,$3,false,$4,$5) ON CONFLICT(definition_uid) DO UPDATE SET name=excluded.name,current_version=excluded.current_version,retired=false,updated_at=excluded.updated_at,operation_id=excluded.operation_id`, req.DefinitionUID, req.Definition.Name, version, now.UnixMilli(), req.OperationID)
		if err != nil {
			return err
		}
		if err := finishOperationTx(ctx, s.db, tx, req.OperationID, core.OperationSuccess, "FLOW_PUBLISHED", "", nil, now); err != nil {
			return err
		}
		result = flow.DefinitionVersion{DefinitionUID: req.DefinitionUID, Version: version, ContentHash: hash, Definition: req.Definition, PublishedAt: now.UnixMilli()}
		if version == current.Version {
			result.PublishedAt = current.PublishedAt
		}
		return nil
	})
	return result, err
}

func (s *FlowStore) Retire(ctx context.Context, req flow.RetireRequest) (flow.DefinitionVersion, error) {
	var result flow.DefinitionVersion
	if !req.Principal.Valid() || req.Principal.Kind != core.PrincipalLocalAdmin {
		return result, core.ErrUnauthorized
	}
	err := s.flowTx(ctx, func(ctx context.Context, q Queryer) (bool, error) {
		return flowOperationCommitted(ctx, q, req.OperationID)
	}, func(ctx context.Context, tx *sql.Tx) error {
		if err := lockFlowDefinitions(ctx, tx); err != nil {
			return err
		}
		current, err := readFlowCurrent(ctx, tx, req.DefinitionUID)
		if err != nil {
			return err
		}
		if current.Version != req.ExpectedVersion || req.Version != current.Version {
			return core.ErrPreconditionFailed
		}
		if err := checkFlowPendingOperation(ctx, tx, req.OperationID, "flow.retire", req.DefinitionUID, current.Version); err != nil {
			return err
		}
		now := s.db.clock()
		if _, err := tx.ExecContext(ctx, `UPDATE flow_pointers SET retired=true,updated_at=$2,operation_id=$3 WHERE definition_uid=$1`, req.DefinitionUID, now.UnixMilli(), req.OperationID); err != nil {
			return err
		}
		if err := finishOperationTx(ctx, s.db, tx, req.OperationID, core.OperationSuccess, "FLOW_RETIRED", "", nil, now); err != nil {
			return err
		}
		current.Retired = true
		result = current
		return nil
	})
	return result, err
}

func lockFlowDefinitions(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, advisoryKey("basalt/flow/definitions"))
	return err
}

func checkFlowPendingOperation(ctx context.Context, tx *sql.Tx, id, action, target string, version int64) error {
	var actualAction, actualTarget, status string
	var actualVersion int64
	err := tx.QueryRowContext(ctx, `SELECT action,target_id,status,requested_version_id FROM management_operations WHERE operation_id=$1 FOR UPDATE`, id).Scan(&actualAction, &actualTarget, &status, &actualVersion)
	if err != nil {
		return err
	}
	if actualAction != action || actualTarget != target || actualVersion != version || status != string(core.OperationPending) {
		return flow.ErrStateConflict
	}
	return nil
}

func flowOperationCommitted(ctx context.Context, q Queryer, operationID string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM management_operations WHERE operation_id=$1 AND status='success')`, operationID).Scan(&exists)
	return exists, err
}

func (s *FlowStore) flowTx(ctx context.Context, check func(context.Context, Queryer) (bool, error), fn func(context.Context, *sql.Tx) error) error {
	outcome, err := s.db.Tx(ctx, fn)
	if outcome != TxUnknown {
		return err
	}
	var unknown *TxUnknownError
	if !errors.As(err, &unknown) {
		return errors.Join(flow.ErrCommitUnknown, err)
	}
	verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	verdict, verifyErr := s.db.VerifyTx(verifyCtx, unknown.Attempt, check)
	if verifyErr == nil && verdict == TxVerifyCommitted {
		return nil
	}
	slog.Warn("Flow transaction outcome unresolved", "reason", "commit_unknown", "verdict", string(verdict))
	return errors.Join(flow.ErrCommitUnknown, err, verifyErr)
}

func flowPageLimit(limit int) (int, error) {
	if limit == 0 {
		return 50, nil
	}
	if limit < 1 || limit > 100 {
		return 0, fmt.Errorf("flow page limit must be between 1 and 100")
	}
	return limit, nil
}
