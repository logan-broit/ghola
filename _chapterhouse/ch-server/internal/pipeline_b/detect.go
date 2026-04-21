// Package pipeline_b implements the nightly cross-user distillation
// from episodic → semantic. Stage 1 (detect) mines the last N hours of
// episodic.events for entity pairs that co-occur in ≥ minSupport
// distinct sessions; Stage 2 (mentat) asks vLLM to distill those
// patterns into a mneme; Stage 3 (upsert) inserts or strengthens the
// matching semantic.mnemes row.
package pipeline_b

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EntityPair is one co-occurring (e1, e2) with support_count and the
// distinct session ids that contributed. e1 < e2 lexicographically so
// the pair is canonical — no duplicate (A,B)/(B,A) rows.
type EntityPair struct {
	E1         string
	E2         string
	Support    int
	SessionIDs []string
}

// DetectPairs scans episodic.events.ingested_at >= now()-window and
// returns entity pairs observed in at least minSupport distinct
// sessions. SessionIDs is the full set that contributed — callers
// feed those to Pipeline B stage 2.
func DetectPairs(ctx context.Context, pool *pgxpool.Pool, window time.Duration, minSupport int) ([]EntityPair, error) {
	if minSupport < 1 {
		minSupport = 1
	}
	rows, err := pool.Query(ctx, `
		WITH pairs AS (
		  SELECT unnest(entities) AS e, session_id
		    FROM episodic.events
		   WHERE ingested_at >= now() - make_interval(secs => $1::double precision)
		     AND entities IS NOT NULL
		)
		SELECT a.e AS e1, b.e AS e2,
		       count(DISTINCT a.session_id) AS support,
		       array_agg(DISTINCT a.session_id::text) AS sessions
		  FROM pairs a
		  JOIN pairs b
		    ON a.session_id = b.session_id
		   AND a.e < b.e
		 GROUP BY 1, 2
		HAVING count(DISTINCT a.session_id) >= $2
		 ORDER BY support DESC, e1, e2
	`, window.Seconds(), minSupport)
	if err != nil {
		return nil, fmt.Errorf("detect pairs: %w", err)
	}
	defer rows.Close()

	var out []EntityPair
	for rows.Next() {
		var p EntityPair
		if err := rows.Scan(&p.E1, &p.E2, &p.Support, &p.SessionIDs); err != nil {
			return nil, fmt.Errorf("scan pair: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
