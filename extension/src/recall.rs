// pg_ghola::recall — v2 composite recall function
//
// Fuses vector similarity, FTS, ACT-R temporal activation, Hebbian
// association strength, and Bayesian confidence into a single ranked
// result set over `semantic.mnemes`. All objects land in the
// `semantic` schema via the control file's schema directive.
//
// v2 removes the sub_mnemes, cluster, and entity-gating pathways from
// v1. Episodic raw turn storage lives in chapterhouse's episodic
// schema now, so recall here only reasons over distilled semantic
// mnemes.

use pgrx::prelude::*;
use std::collections::HashMap;

use crate::scoring::{actr_activation_inner, softplus_inner};

/// One-minute floor in days (1/1440). Prevents divide-by-zero for
/// extremely recent last_access values in ACT-R decay.
const ONE_MINUTE_DAYS: f64 = 1.0 / 1440.0;

// ---------------------------------------------------------------------------
// SQL wrapper: recall()
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE FUNCTION recall(
    workspace_id uuid,
    query_text text,
    query_embedding vector,
    limit_n int DEFAULT 10,
    min_confidence float8 DEFAULT 0.0,
    weights semantic.score_weights DEFAULT NULL,
    memory_type text DEFAULT NULL,
    tags text[] DEFAULT NULL,
    filter_entities text[] DEFAULT NULL
) RETURNS SETOF semantic.recall_result
LANGUAGE SQL
STABLE
AS $$
    SELECT (mneme_id, score, content_match, activation, hebbian_boost,
            confidence, concept, content)::semantic.recall_result
    FROM semantic.recall_inner(
        workspace_id, query_text, query_embedding::text, limit_n, min_confidence,
        COALESCE((weights).semantic, 0.6),
        COALESCE((weights).fts, 0.4),
        COALESCE((weights).actr_decay, 0.5),
        COALESCE((weights).hebbian_scale, 4.0),
        memory_type, tags, filter_entities
    );
$$;
"#,
    name = "create_recall_wrapper",
    requires = [
        recall_inner,
        "create_type_recall_result",
        "create_type_score_weights",
        "create_semantic_schema_and_mnemes",
        "create_associations_table",
        "create_queue_tables",
    ],
);

// ---------------------------------------------------------------------------
// Internal recall implementation
// ---------------------------------------------------------------------------

/// Candidate fetched from `semantic.mnemes` before scoring.
struct Candidate {
    id: pgrx::Uuid,
    concept: String,
    content: String,
    confidence: f64,
    access_count: i32,
    age_days: f64,
    cosine_sim: f64,
    fts_rank: f64,
    memory_type: String,
}

/// Scored candidate ready for output.
struct ScoredCandidate {
    id: pgrx::Uuid,
    score: f64,
    content_match: f64,
    activation: f64,
    hebbian_boost: f64,
    confidence: f64,
    concept: String,
    content: String,
}

/// Internal implementation of the recall function. Parameters are
/// decomposed (weights as individual floats, embedding as text) to
/// side-step pgrx limitations with pgvector + composite types.
#[pg_extern(stable)]
fn recall_inner(
    workspace_id: pgrx::Uuid,
    query_text: &str,
    query_embedding_text: &str,
    limit_n: default!(i32, 10),
    min_confidence: default!(f64, 0.0),
    w_semantic: default!(f64, 0.6),
    w_fts: default!(f64, 0.4),
    w_actr_decay: default!(f64, 0.5),
    w_hebbian_scale: default!(f64, 4.0),
    filter_memory_type: default!(Option<String>, "NULL"),
    filter_tags: default!(Option<Vec<String>>, "NULL"),
    filter_entities: default!(Option<Vec<String>>, "NULL"),
) -> TableIterator<
    'static,
    (
        name!(mneme_id, pgrx::Uuid),
        name!(score, f64),
        name!(content_match, f64),
        name!(activation, f64),
        name!(hebbian_boost, f64),
        name!(confidence, f64),
        name!(concept, String),
        name!(content, String),
    ),
> {
    let pool_size = 3 * limit_n;
    let escaped_text = query_text.replace('\'', "''");

    // Build optional filter clauses
    let mut extra_filters = String::new();
    if let Some(ref mt) = filter_memory_type {
        extra_filters.push_str(&format!(" AND memory_type = '{}'", mt.replace('\'', "''")));
    }
    if let Some(ref tags) = filter_tags {
        if !tags.is_empty() {
            let tag_literals: Vec<String> = tags
                .iter()
                .map(|t| format!("'{}'", t.replace('\'', "''")))
                .collect();
            extra_filters.push_str(&format!(
                " AND tags @> ARRAY[{}]::text[]",
                tag_literals.join(",")
            ));
        }
    }
    if let Some(ref ents) = filter_entities {
        if !ents.is_empty() {
            let ent_literals: Vec<String> = ents
                .iter()
                .map(|e| format!("'{}'", e.replace('\'', "''")))
                .collect();
            extra_filters.push_str(&format!(
                " AND entities && ARRAY[{}]::text[]",
                ent_literals.join(",")
            ));
        }
    }

    // Step 1: candidate pool over semantic + lexical pathways.
    let candidates: Vec<Candidate> = Spi::connect(|client| {
        let query = format!(
            "WITH \
            semantic_pool AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type \
                FROM semantic.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                ORDER BY embedding <=> '{emb}'::vector \
                LIMIT {pool} \
            ), \
            lexical_pool AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type \
                FROM semantic.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                  AND search_vector @@ plainto_tsquery('english', '{qt}') \
                ORDER BY ts_rank(search_vector, plainto_tsquery('english', '{qt}')) DESC \
                LIMIT {pool} \
            ) \
            SELECT DISTINCT ON (id) id, concept, content, confidence, access_count, \
                   age_days, cosine_sim, fts_rank, memory_type \
            FROM ( \
                SELECT * FROM semantic_pool \
                UNION ALL \
                SELECT * FROM lexical_pool \
            ) combined \
            ORDER BY id, cosine_sim DESC",
            ws = workspace_id,
            qt = escaped_text,
            emb = query_embedding_text,
            min_conf = min_confidence,
            min_age = ONE_MINUTE_DAYS,
            pool = pool_size,
            filters = extra_filters,
        );

        let rows = client
            .select(&query, None, &[])
            .expect("failed to query candidate pool");

        let mut result = Vec::new();
        for row in rows {
            result.push(Candidate {
                id: row.get::<pgrx::Uuid>(1).expect("err").expect("null id"),
                concept: row.get::<String>(2).expect("err").expect("null concept"),
                content: row.get::<String>(3).expect("err").expect("null content"),
                confidence: row.get::<f64>(4).expect("err").expect("null confidence"),
                access_count: row
                    .get::<i32>(5)
                    .expect("err")
                    .expect("null access_count"),
                age_days: row.get::<f64>(6).expect("err").unwrap_or(ONE_MINUTE_DAYS),
                cosine_sim: row.get::<f64>(7).expect("err").unwrap_or(0.0),
                fts_rank: row.get::<f64>(8).expect("err").unwrap_or(0.0),
                memory_type: row.get::<String>(9).expect("err").unwrap_or("factual".to_string()),
            });
        }
        result
    });

    // Step 2: typed associations between candidates for Hebbian boost.
    let candidate_ids: Vec<String> = candidates.iter().map(|c| c.id.to_string()).collect();

    let hebbian_boosts: HashMap<String, f64> = if candidates.len() > 1 {
        Spi::connect(|client| {
            let id_list: String = candidate_ids
                .iter()
                .map(|id| format!("'{id}'::uuid"))
                .collect::<Vec<_>>()
                .join(",");

            let query = format!(
                "SELECT src_id, dst_id, association_type, weight FROM semantic.associations \
                 WHERE src_id IN ({ids}) AND dst_id IN ({ids})",
                ids = id_list,
            );

            let rows = client
                .select(&query, None, &[])
                .expect("failed to query associations for boost");

            let mut boosts: HashMap<String, f64> = HashMap::new();
            for row in rows {
                let src: pgrx::Uuid = row.get(1).expect("err").expect("null src_id");
                let dst: pgrx::Uuid = row.get(2).expect("err").expect("null dst_id");
                let assoc_type: String = row.get::<String>(3).expect("err").expect("null type");
                let weight: f64 = row.get(4).expect("err").expect("null weight");

                // Type-aware boost scaling matches v1 semantics minus the
                // retired 'session' bucket.
                let scaled = match assoc_type.as_str() {
                    "hebbian" => weight,
                    "supports" => weight * 0.5,
                    "contradicts" => -weight * 0.5,
                    _ => 0.0, // supersedes + unknown types don't contribute
                };

                *boosts.entry(src.to_string()).or_insert(0.0) += scaled;
                *boosts.entry(dst.to_string()).or_insert(0.0) += scaled;
            }
            boosts
        })
    } else {
        HashMap::new()
    };

    // Step 3: composite scores.
    let softplus_0 = softplus_inner(0.0);
    let normalizer = 1.0 + softplus_0;

    let mut scored: Vec<ScoredCandidate> = candidates
        .into_iter()
        .map(|c| {
            let content_match = w_semantic * c.cosine_sim + w_fts * c.fts_rank.tanh();

            // memory_type-aware ACT-R decay kept from v1.
            let effective_decay = match c.memory_type.as_str() {
                "experiential" => (w_actr_decay * 2.0).min(1.5),
                _ => w_actr_decay,
            };
            let actr_val = actr_activation_inner(c.access_count, c.age_days, effective_decay);

            let heb_boost = hebbian_boosts
                .get(&c.id.to_string())
                .copied()
                .unwrap_or(0.0);

            let temporal_weight =
                softplus_inner(actr_val + w_hebbian_scale * heb_boost) / normalizer;
            let score = content_match * temporal_weight * c.confidence;

            ScoredCandidate {
                id: c.id,
                score,
                content_match,
                activation: actr_val,
                hebbian_boost: heb_boost,
                confidence: c.confidence,
                concept: c.concept,
                content: c.content,
            }
        })
        .collect();

    // Step 4: sort by composite score descending and truncate.
    scored.sort_by(|a, b| {
        b.score
            .partial_cmp(&a.score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    scored.truncate(limit_n as usize);

    // Step 5: enqueue co-activation pairs so the Hebbian worker can
    // strengthen associations. v2 queue stores (src_id, dst_id) pairs
    // rather than v1's array-per-recall.
    enqueue_co_activation_pairs(&scored);

    // Step 6: emit 8-column rows.
    let results: Vec<_> = scored
        .into_iter()
        .map(|s| {
            (
                s.id,
                s.score,
                s.content_match,
                s.activation,
                s.hebbian_boost,
                s.confidence,
                s.concept,
                s.content,
            )
        })
        .collect();

    TableIterator::new(results)
}

/// Insert one row per unordered pair of returned mnemes into
/// `semantic.co_activation_queue`. The Hebbian worker drains this
/// queue and updates `semantic.associations(src_id, dst_id, 'hebbian')`.
fn enqueue_co_activation_pairs(results: &[ScoredCandidate]) {
    if results.len() < 2 {
        return;
    }

    let mut values: Vec<String> = Vec::new();
    for i in 0..results.len() {
        for j in (i + 1)..results.len() {
            let a = &results[i].id.to_string();
            let b = &results[j].id.to_string();
            let (src, dst) = if a < b { (a, b) } else { (b, a) };
            values.push(format!("('{src}'::uuid, '{dst}'::uuid)"));
        }
    }

    Spi::run(&format!(
        "INSERT INTO semantic.co_activation_queue (src_id, dst_id) VALUES {}",
        values.join(",")
    ))
    .expect("failed to enqueue co-activation pairs");
}
