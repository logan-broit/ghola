// pg_ghola::hebbian — v2 Hebbian learning and confidence functions
//
// Drains `semantic.co_activation_queue` (one row per unordered mneme
// pair, inserted by `recall`), strengthens the corresponding `hebbian`
// associations in `semantic.associations`, and updates access tracking
// on the referenced mnemes.
//
// Also exports: get_associations, update_confidence, confirm_recall.
//
// v2 drops the v1 record_co_activation function (the queue enqueue
// path is private now — only recall writes pairs) and removes all
// `tier` column references (core / index / state buckets retired).

use pgrx::prelude::*;

use crate::scoring::bayesian_update_inner;

// ---------------------------------------------------------------------------
// get_associations
// ---------------------------------------------------------------------------

/// Associations touching a mneme (both directions), filtered by
/// minimum weight, ordered descending by weight.
#[pg_extern]
fn get_associations(
    mneme_id: pgrx::Uuid,
    min_weight: default!(f64, 0.01),
) -> TableIterator<'static, (name!(related_id, pgrx::Uuid), name!(weight, f64))> {
    let results = Spi::connect(|client| {
        let query = format!(
            "SELECT related_id, weight FROM ( \
                SELECT dst_id AS related_id, weight FROM semantic.associations \
                WHERE src_id = '{mneme_id}' AND weight >= {min_weight} \
                UNION ALL \
                SELECT src_id AS related_id, weight FROM semantic.associations \
                WHERE dst_id = '{mneme_id}' AND weight >= {min_weight} \
            ) sub \
            ORDER BY weight DESC"
        );

        let tup_table = client
            .select(&query, None, &[])
            .expect("failed to query associations");

        let mut results = Vec::new();
        for row in tup_table {
            let related_id: pgrx::Uuid = row
                .get::<pgrx::Uuid>(1)
                .expect("failed to get related_id")
                .expect("null related_id");
            let weight: f64 = row
                .get::<f64>(2)
                .expect("failed to get weight")
                .expect("null weight");
            results.push((related_id, weight));
        }
        results
    });

    TableIterator::new(results)
}

// ---------------------------------------------------------------------------
// update_confidence / confirm_recall
// ---------------------------------------------------------------------------

/// Single confidence floor (Laplace bound) used throughout v2. v1's
/// per-tier floor (core/index/state) is gone with the tier column.
const CONFIDENCE_FLOOR: f64 = 0.025;

/// Atomically read a mneme's current confidence, apply
/// bayesian_update with `evidence`, write back the clamped posterior,
/// return it.
#[pg_extern]
fn update_confidence(mneme_id: pgrx::Uuid, evidence: f64) -> f64 {
    Spi::connect_mut(|client| {
        let tup_table = client
            .select(
                &format!(
                    "SELECT confidence FROM semantic.mnemes \
                     WHERE id = '{mneme_id}' FOR UPDATE"
                ),
                None,
                &[],
            )
            .expect("failed to read mneme confidence");

        let row = tup_table.into_iter().next();
        match row {
            None => pgrx::error!("mneme_id not found in mnemes table: {mneme_id}"),
            Some(r) => {
                let prior: f64 = r
                    .get::<f64>(1)
                    .expect("failed to get confidence")
                    .expect("null confidence");

                let posterior = bayesian_update_inner(prior, evidence);
                let clamped = posterior.max(CONFIDENCE_FLOOR);

                client
                    .update(
                        &format!(
                            "UPDATE semantic.mnemes SET confidence = {clamped} \
                             WHERE id = '{mneme_id}'"
                        ),
                        None,
                        &[],
                    )
                    .expect("failed to update confidence");

                clamped
            }
        }
    })
}

/// Apply strong confirmation (evidence = 0.95) to each mneme.
#[pg_extern]
fn confirm_recall(mneme_ids: Vec<pgrx::Uuid>) -> &'static str {
    for id in &mneme_ids {
        Spi::connect_mut(|client| {
            let tup_table = client
                .select(
                    &format!(
                        "SELECT confidence FROM semantic.mnemes \
                         WHERE id = '{id}' FOR UPDATE"
                    ),
                    None,
                    &[],
                )
                .expect("failed to read mneme confidence");

            let row = tup_table.into_iter().next();
            match row {
                None => pgrx::error!("mneme_id not found in mnemes table: {id}"),
                Some(r) => {
                    let prior: f64 = r
                        .get::<f64>(1)
                        .expect("failed to get confidence")
                        .expect("null confidence");

                    let posterior = bayesian_update_inner(prior, 0.95);
                    let clamped = posterior.max(CONFIDENCE_FLOOR);

                    client
                        .update(
                            &format!(
                                "UPDATE semantic.mnemes SET confidence = {clamped} \
                                 WHERE id = '{id}'"
                            ),
                            None,
                            &[],
                        )
                        .expect("failed to update confidence");
                }
            }
        });
    }
    "ok"
}

// ---------------------------------------------------------------------------
// Co-activation batch processing
// ---------------------------------------------------------------------------

/// Drain up to `batch_limit` rows from `semantic.co_activation_queue`
/// (each row is one (src_id, dst_id) pair) and fold the events into
/// `semantic.associations` as `hebbian` links with log-space weight
/// growth.
///
/// Each queue row is one co-activation observation. If the same pair
/// appears N times in the batch, its signal is N — the log-space
/// update grows weight by `exp(N * ln(1.01))` bounded at 1.0.
#[pg_extern]
fn process_co_activation_batch(batch_limit: default!(i32, 100)) -> i64 {
    use std::collections::{HashMap, HashSet};

    Spi::connect_mut(|client| {
        let rows = client
            .select(
                &format!(
                    "SELECT id, src_id, dst_id FROM semantic.co_activation_queue \
                     ORDER BY id LIMIT {batch_limit}"
                ),
                None,
                &[],
            )
            .expect("failed to read co_activation_queue");

        struct Event {
            id: i64,
            src: String,
            dst: String,
        }

        let mut events: Vec<Event> = Vec::new();
        for row in rows {
            let id: i64 = row.get::<i64>(1).expect("err").expect("null id");
            let src: pgrx::Uuid = row.get::<pgrx::Uuid>(2).expect("err").expect("null src_id");
            let dst: pgrx::Uuid = row.get::<pgrx::Uuid>(3).expect("err").expect("null dst_id");
            events.push(Event {
                id,
                src: src.to_string(),
                dst: dst.to_string(),
            });
        }

        if events.is_empty() {
            return 0i64;
        }

        let num_processed = events.len() as i64;

        // Aggregate pair counts. The enqueue side already uses canonical
        // src < dst ordering, but we re-canonicalize defensively here in
        // case direct INSERTs bypass that rule.
        let mut pair_counts: HashMap<(String, String), i64> = HashMap::new();
        let mut all_mneme_ids: HashSet<String> = HashSet::new();
        let mut consumed_ids: Vec<i64> = Vec::with_capacity(events.len());

        for ev in &events {
            consumed_ids.push(ev.id);
            all_mneme_ids.insert(ev.src.clone());
            all_mneme_ids.insert(ev.dst.clone());
            let (a, b) = if ev.src <= ev.dst {
                (ev.src.clone(), ev.dst.clone())
            } else {
                (ev.dst.clone(), ev.src.clone())
            };
            *pair_counts.entry((a, b)).or_insert(0) += 1;
        }

        // Upsert associations: log-space weight growth capped at 1.0.
        // Seed new associations at 0.01 before the reinforcement step.
        for ((src, dst), count) in &pair_counts {
            let signal = *count as f64;
            client
                .update(
                    &format!(
                        "INSERT INTO semantic.associations \
                             (src_id, dst_id, association_type, weight, \
                              co_activations, updated_at) \
                         VALUES ('{src}', '{dst}', 'hebbian', \
                             LEAST(1.0, EXP(LN(0.01) + {signal} * LN(1.01))), \
                             {count}::int, now()) \
                         ON CONFLICT (src_id, dst_id, association_type) DO UPDATE SET \
                             weight = LEAST(1.0, EXP(LN(semantic.associations.weight) + {signal} * LN(1.01))), \
                             co_activations = semantic.associations.co_activations + {count}::int, \
                             updated_at = now()"
                    ),
                    None,
                    &[],
                )
                .expect("failed to upsert hebbian association");
        }

        // Bump access_count / last_access on every referenced mneme.
        for mid in &all_mneme_ids {
            client
                .update(
                    &format!(
                        "UPDATE semantic.mnemes \
                         SET access_count = access_count + 1, \
                             last_access = now() \
                         WHERE id = '{mid}'"
                    ),
                    None,
                    &[],
                )
                .expect("failed to update access tracking");
        }

        // Drop consumed queue rows.
        let id_list: String = consumed_ids
            .iter()
            .map(|id| id.to_string())
            .collect::<Vec<_>>()
            .join(",");
        client
            .update(
                &format!(
                    "DELETE FROM semantic.co_activation_queue WHERE id IN ({id_list})"
                ),
                None,
                &[],
            )
            .expect("failed to delete consumed queue rows");

        num_processed
    })
}

/// Drain the queue completely by repeated batch processing.
#[pg_extern]
fn process_all_pending_co_activations() -> i64 {
    let mut total: i64 = 0;
    loop {
        let processed = Spi::get_one::<i64>(
            "SELECT semantic.process_co_activation_batch(100)",
        )
        .expect("failed to call process_co_activation_batch")
        .unwrap_or(0);

        if processed == 0 {
            break;
        }
        total += processed;
    }
    total
}
