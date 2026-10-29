package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	storedb "github.com/SnapdragonPartners/buggrit/internal/store/db"
	"github.com/SnapdragonPartners/buggrit/internal/telemetry"
)

// Telemetry returns the sink for run telemetry over this store's
// connections (design §6, "Telemetry"). It stays usable until Close.
func (s *Store) Telemetry() telemetry.Sink { return telemetrySink{q: storedb.New(s.pool)} }

type telemetrySink struct{ q *storedb.Queries }

func (t telemetrySink) StartRun(ctx context.Context, run *telemetry.Run) error {
	id, err := uuid(run.ID)
	if err != nil {
		return err
	}
	meta, err := json.Marshal(run.Meta)
	if err != nil {
		return fmt.Errorf("encode the run's meta: %w", err)
	}
	err = t.q.StartRun(ctx, storedb.StartRunParams{
		ID: id, StartedAt: timestamptz(run.StartedAt), Repo: run.Repo, Pr: int32(run.PR), //nolint:gosec // a PR number fits
		HeadSha: run.HeadSHA, DryRun: run.DryRun, Label: run.Label, Meta: meta,
	})
	if err != nil {
		return fmt.Errorf("start the run: %w", err)
	}
	return nil
}

func (t telemetrySink) WriteEvents(ctx context.Context, runID string, events []*telemetry.Event) error {
	id, err := uuid(runID)
	if err != nil {
		return err
	}
	arg := storedb.InsertEventsParams{
		RunID:    id,
		Seqs:     make([]int32, len(events)),
		Ats:      make([]pgtype.Timestamptz, len(events)),
		Kinds:    make([]string, len(events)),
		Sessions: make([]string, len(events)),
		Calls:    make([]int32, len(events)),
		Payloads: make([]string, len(events)),
	}
	for i, e := range events {
		arg.Seqs[i], arg.Calls[i] = int32(e.Seq), int32(e.Call) //nolint:gosec // counts within one run
		arg.Ats[i], arg.Kinds[i], arg.Sessions[i], arg.Payloads[i] = timestamptz(e.At), e.Kind, e.Session, string(e.Payload)
	}
	if err := t.q.InsertEvents(ctx, arg); err != nil {
		return fmt.Errorf("write events: %w", err)
	}
	return nil
}

func (t telemetrySink) FinishRun(ctx context.Context, runID string, end *telemetry.End) error {
	id, err := uuid(runID)
	if err != nil {
		return err
	}
	outcome, err := json.Marshal(end.Outcome)
	if err != nil {
		return fmt.Errorf("encode the run's outcome: %w", err)
	}
	err = t.q.FinishRun(ctx, storedb.FinishRunParams{
		ID: id, EndedAt: timestamptz(end.EndedAt), Outcome: outcome,
		EventsEmitted: pgtype.Int4{Int32: int32(end.Emitted), Valid: true}, //nolint:gosec // counts within one run
		EventsWritten: pgtype.Int4{Int32: int32(end.Written), Valid: true}, //nolint:gosec // as above
		Complete:      end.Complete,
	})
	if err != nil {
		return fmt.Errorf("finish the run: %w", err)
	}
	return nil
}

func uuid(s string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		return id, fmt.Errorf("run ID %q: %w", s, err)
	}
	return id, nil
}

func timestamptz(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }
