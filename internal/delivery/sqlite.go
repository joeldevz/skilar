package delivery

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type SQLiteIntentStore struct{ db *sql.DB }

func NewSQLiteIntentStore(db *sql.DB) *SQLiteIntentStore { return &SQLiteIntentStore{db: db} }
func (s *SQLiteIntentStore) Get(workflowID, key string) (Intent, bool) {
	var raw []byte
	err := s.db.QueryRow(`SELECT intent FROM delivery_intents WHERE workflow_id=? AND idempotency_key=?`, workflowID, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Intent{}, false
	}
	if err != nil {
		return Intent{}, false
	}
	var i Intent
	if json.Unmarshal(raw, &i) != nil {
		return Intent{}, false
	}
	return i, true
}
func (s *SQLiteIntentStore) Put(i Intent) error {
	raw, _ := json.Marshal(i)
	res, err := s.db.Exec(`INSERT OR IGNORE INTO delivery_intents(workflow_id,idempotency_key,intent) VALUES(?,?,?)`, i.WorkflowID, i.IdempotencyKey, raw)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	old, ok := s.Get(i.WorkflowID, i.IdempotencyKey)
	if !ok {
		return errors.New("delivery: corrupt persisted intent")
	}
	if old == i {
		return nil
	}
	return ErrIdempotencyReuse
}
