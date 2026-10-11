// Package httpapi exposes the ledger over JSON/HTTP.
package httpapi

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/michael-jw-ji/go-ledger/internal/ledger"
)

const (
	maxBodyBytes      = 1 << 20
	defaultPageSize   = 50
	maxPageSize       = 500
	idempotencyHeader = "Idempotency-Key"
	replayedHeader    = "Idempotent-Replayed"
)

type Server struct {
	svc *ledger.Service
	log *slog.Logger
}

func New(svc *ledger.Service, log *slog.Logger) http.Handler {
	s := &Server{svc: svc, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST /accounts", s.createAccount)
	mux.HandleFunc("GET /accounts", s.listAccounts)
	mux.HandleFunc("GET /accounts/{id}", s.getAccount)
	mux.HandleFunc("GET /accounts/{id}/entries", s.listEntries)
	mux.HandleFunc("POST /transfers", s.createTransfer)
	mux.HandleFunc("GET /transfers/{id}", s.getTransfer)
	mux.HandleFunc("POST /transfers/{id}/reverse", s.reverseTransfer)
	return mux
}

func (s *Server) createAccount(w http.ResponseWriter, r *http.Request) {
	var in ledger.NewAccount
	if !decode(w, r, &in) {
		return
	}
	a, err := s.svc.CreateAccount(r.Context(), in)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	accts, err := s.svc.ListAccounts(r.Context())
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accts})
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	a, err := s.svc.GetAccount(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	id, ok := accountID(w, r)
	if !ok {
		return
	}
	limit := defaultPageSize
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxPageSize {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 500")
			return
		}
		limit = n
	}
	before, err := decodeCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor is malformed")
		return
	}
	entries, next, err := s.svc.ListEntries(r.Context(), id, before, limit)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	resp := map[string]any{"entries": entries, "next_cursor": nil}
	if next != 0 {
		resp["next_cursor"] = encodeCursor(next)
	}
	writeJSON(w, http.StatusOK, resp)
}

// transferBody accepts either explicit entries or the two-leg shorthand.
type transferBody struct {
	Entries         []ledger.Leg `json:"entries"`
	DebitAccountID  int64        `json:"debit_account_id"`
	CreditAccountID int64        `json:"credit_account_id"`
	Amount          int64        `json:"amount"`
}

func (b transferBody) legs() []ledger.Leg {
	if len(b.Entries) > 0 {
		return b.Entries
	}
	return []ledger.Leg{
		{AccountID: b.DebitAccountID, Direction: ledger.Debit, Amount: b.Amount},
		{AccountID: b.CreditAccountID, Direction: ledger.Credit, Amount: b.Amount},
	}
}

func (s *Server) createTransfer(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	var body transferBody
	if !decode(w, r, &body) {
		return
	}
	if len(body.Entries) > 0 && (body.DebitAccountID != 0 || body.CreditAccountID != 0 || body.Amount != 0) {
		writeError(w, http.StatusUnprocessableEntity, "validation_error",
			"send either entries or debit_account_id/credit_account_id/amount, not both")
		return
	}
	t, err := s.svc.CreateTransfer(r.Context(), ledger.TransferRequest{IdempotencyKey: key, Legs: body.legs()})
	s.writeTransfer(w, r, t, err)
}

func (s *Server) getTransfer(w http.ResponseWriter, r *http.Request) {
	id, ok := transferID(w, r)
	if !ok {
		return
	}
	t, err := s.svc.GetTransfer(r.Context(), id)
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) reverseTransfer(w http.ResponseWriter, r *http.Request) {
	key, ok := requireKey(w, r)
	if !ok {
		return
	}
	id, ok := transferID(w, r)
	if !ok {
		return
	}
	t, err := s.svc.ReverseTransfer(r.Context(), id, key)
	s.writeTransfer(w, r, t, err)
}

// writeTransfer answers a fresh or replayed transfer identically, so a
// client can't tell (except by the replay header) whether its retry posted.
func (s *Server) writeTransfer(w http.ResponseWriter, r *http.Request, t ledger.Transfer, err error) {
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	if t.Replayed {
		w.Header().Set(replayedHeader, "true")
	}
	if t.Status == ledger.StatusRejected {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    apiError{Code: string(t.ErrorCode), Message: t.ErrorCode.Message()},
			"transfer": t,
		})
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ledger.ErrInvalidRequest):
		writeError(w, http.StatusUnprocessableEntity, "validation_error", err.Error())
	case errors.Is(err, ledger.ErrUnbalanced):
		writeError(w, http.StatusUnprocessableEntity, "unbalanced", err.Error())
	case errors.Is(err, ledger.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ledger.ErrIdempotencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	case errors.Is(err, ledger.ErrAlreadyReversed):
		writeError(w, http.StatusConflict, "already_reversed", err.Error())
	case errors.Is(err, ledger.ErrNotReversible):
		writeError(w, http.StatusUnprocessableEntity, "not_reversible", err.Error())
	default:
		s.log.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "malformed_json", err.Error())
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "malformed_json", "body must be a single JSON object")
		return false
	}
	return true
}

func requireKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing_idempotency_key", "the Idempotency-Key header is required")
		return "", false
	}
	return key, true
}

func accountID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return 0, false
	}
	return id, true
}

func transferID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "transfer not found")
		return uuid.Nil, false
	}
	return id, true
}

// Cursors are opaque to clients so the pagination key can change later.
func encodeCursor(id int64) string {
	return base64.RawURLEncoding.EncodeToString(binary.BigEndian.AppendUint64(nil, uint64(id)))
}

func decodeCursor(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) != 8 {
		return 0, errors.New("bad cursor")
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}
