package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
)

// ExceptionDetail is an exception with its evidence and full audit trail.
type ExceptionDetail struct {
	sqlite.Exception
	Evidence json.RawMessage         `json:"evidence,omitempty"`
	History  []sqlite.ExceptionEvent `json:"history"`
}

// Exceptions lists the exception queue.
func (s *Service) Exceptions(ctx context.Context, f sqlite.ExceptionFilter) ([]sqlite.Exception, error) {
	return s.store.Exceptions(ctx, f)
}

// Exception returns one exception with evidence and audit trail.
func (s *Service) Exception(ctx context.Context, id int64) (*ExceptionDetail, error) {
	e, hist, err := s.store.Exception(ctx, id)
	if err != nil {
		return nil, err
	}
	d := &ExceptionDetail{Exception: e, History: hist}
	if ev, err := s.store.Evidence(ctx, e.RunID, e.OutcomeID); err == nil && ev != "" {
		d.Evidence = json.RawMessage(ev)
	}
	return d, nil
}

// Resolve records a human decision on an open exception. actor is who
// decided (the authenticated principal over HTTP, -by on the CLI); reason is
// mandatory - "fixed" without a why is not an audit trail.
func (s *Service) Resolve(ctx context.Context, id int64, actor, reason string) (sqlite.Exception, error) {
	return s.transition(ctx, id, sqlite.ActionResolved, actor, reason)
}

// Reopen puts a resolved or auto-closed exception back in the queue.
func (s *Service) Reopen(ctx context.Context, id int64, actor, reason string) (sqlite.Exception, error) {
	return s.transition(ctx, id, sqlite.ActionReopened, actor, reason)
}

func (s *Service) transition(ctx context.Context, id int64, action, actor, reason string) (sqlite.Exception, error) {
	actor, reason = strings.TrimSpace(actor), strings.TrimSpace(reason)
	switch {
	case actor == "" || utf8.RuneCountInString(actor) > 100:
		return sqlite.Exception{}, fmt.Errorf("%w: actor required (max 100 chars)", ErrValidation)
	case actor == sqlite.SystemActor:
		return sqlite.Exception{}, fmt.Errorf("%w: actor %q is reserved", ErrValidation, actor)
	case reason == "" || utf8.RuneCountInString(reason) > 1000:
		return sqlite.Exception{}, fmt.Errorf("%w: reason required (max 1000 chars)", ErrValidation)
	}
	e, err := s.store.Transition(ctx, id, action, actor, reason)
	if err != nil {
		return e, err
	}
	if c, err := s.store.ExceptionCounts(ctx); err == nil {
		s.metrics.ExceptionsOpen(c[sqlite.StatusOpen])
	}
	s.log.Info("exception "+action, "id", id, "actor", actor)
	return e, nil
}
