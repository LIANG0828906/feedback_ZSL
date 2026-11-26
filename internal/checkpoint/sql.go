package checkpoint

import (
	"encoding/json"
	"fmt"
	"strings"

	"quota-cluster/internal/bizerror"
	"quota-cluster/internal/model"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS checkpoint_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS committed_tick (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  tick INTEGER NOT NULL,
  format_version TEXT NOT NULL,
  project_id TEXT NOT NULL,
  project_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tenant_state (
  tenant_id TEXT PRIMARY KEY,
  tick INTEGER NOT NULL,
  used_bytes INTEGER NOT NULL,
  available_quota_bytes INTEGER NOT NULL,
  guarantee_remaining_bytes INTEGER NOT NULL,
  payload_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS node_state (
  node_id TEXT PRIMARY KEY,
  tick INTEGER NOT NULL,
  capacity_bytes INTEGER NOT NULL,
  used_bytes INTEGER NOT NULL,
  held_bytes INTEGER NOT NULL,
  free_bytes INTEGER NOT NULL,
  payload_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS queue_member (
  request_id TEXT PRIMARY KEY,
  tenant_id TEXT NOT NULL,
  node_id TEXT NOT NULL,
  position INTEGER NOT NULL,
  remaining_bytes INTEGER NOT NULL,
  payload_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS decision_record (
  request_id TEXT NOT NULL,
  tick INTEGER NOT NULL,
  verdict TEXT NOT NULL,
  preheld_bytes INTEGER NOT NULL,
  correction_bytes INTEGER NOT NULL,
  payload_json TEXT NOT NULL,
  PRIMARY KEY (tick, request_id)
);
CREATE TABLE IF NOT EXISTS engine_state (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  tick INTEGER NOT NULL,
  payload_json TEXT NOT NULL
);
`

func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func jsonQuote(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return sqlQuote(string(b)), nil
}

func buildSaveStatements(cp *Checkpoint, projectHash string) (pre []string, global string, err error) {
	if cp == nil {
		return nil, "", bizerror.New(bizerror.CodeCheckpointCorrupt, "/checkpoint", "nil checkpoint")
	}
	format := cp.FormatVersion
	if format == "" {
		format = model.FormatVersion
	}
	hash := projectHash
	if hash == "" {
		hash = cp.ProjectHash
	}

	pre = append(pre,
		"DELETE FROM checkpoint_meta",
		"DELETE FROM tenant_state",
		"DELETE FROM node_state",
		"DELETE FROM queue_member",
		"DELETE FROM decision_record",
		"DELETE FROM engine_state",
		fmt.Sprintf("INSERT INTO checkpoint_meta(key,value) VALUES('format_version',%s)", sqlQuote(format)),
		fmt.Sprintf("INSERT INTO checkpoint_meta(key,value) VALUES('project_hash',%s)", sqlQuote(hash)),
	)

	for _, t := range cp.TenantStates {
		payload, mErr := jsonQuote(t)
		if mErr != nil {
			err = mErr
			return
		}
		pre = append(pre, fmt.Sprintf(
			"INSERT INTO tenant_state(tenant_id,tick,used_bytes,available_quota_bytes,guarantee_remaining_bytes,payload_json) VALUES(%s,%d,%d,%d,%d,%s)",
			sqlQuote(t.TenantID), cp.CommittedTick, t.UsedBytes, t.AvailableQuotaBytes, t.GuaranteeRemainingBytes, payload,
		))
	}
	for _, n := range cp.NodeStates {
		payload, mErr := jsonQuote(n)
		if mErr != nil {
			err = mErr
			return
		}
		pre = append(pre, fmt.Sprintf(
			"INSERT INTO node_state(node_id,tick,capacity_bytes,used_bytes,held_bytes,free_bytes,payload_json) VALUES(%s,%d,%d,%d,%d,%d,%s)",
			sqlQuote(n.NodeID), cp.CommittedTick, n.CapacityBytes, n.UsedBytes, n.HeldBytes, n.FreeBytes, payload,
		))
	}
	for _, q := range cp.Queue {
		payload, mErr := jsonQuote(q)
		if mErr != nil {
			err = mErr
			return
		}
		pre = append(pre, fmt.Sprintf(
			"INSERT INTO queue_member(request_id,tenant_id,node_id,position,remaining_bytes,payload_json) VALUES(%s,%s,%s,%d,%d,%s)",
			sqlQuote(q.RequestID), sqlQuote(q.TenantID), sqlQuote(q.NodeID), q.Position, q.RemainingBytes, payload,
		))
	}
	for _, d := range cp.Decisions {
		payload, mErr := jsonQuote(d)
		if mErr != nil {
			err = mErr
			return
		}
		pre = append(pre, fmt.Sprintf(
			"INSERT INTO decision_record(request_id,tick,verdict,preheld_bytes,correction_bytes,payload_json) VALUES(%s,%d,%s,%d,%d,%s)",
			sqlQuote(d.RequestID), d.Tick, sqlQuote(string(d.Verdict)), d.PreheldBytes, d.SettlementCorrection, payload,
		))
	}
	if cp.EngineState != nil {
		payload, mErr := jsonQuote(cp.EngineState)
		if mErr != nil {
			err = mErr
			return
		}
		pre = append(pre, fmt.Sprintf("INSERT INTO engine_state(id,tick,payload_json) VALUES(1,%d,%s)", cp.CommittedTick, payload))
	}

	global = fmt.Sprintf(`
INSERT INTO committed_tick(id,tick,format_version,project_id,project_hash)
VALUES(1,%d,%s,%s,%s)
ON CONFLICT(id) DO UPDATE SET
  tick=excluded.tick,
  format_version=excluded.format_version,
  project_id=excluded.project_id,
  project_hash=excluded.project_hash`,
		cp.CommittedTick, sqlQuote(format), sqlQuote(cp.ProjectID), sqlQuote(hash))
	return pre, global, nil
}
