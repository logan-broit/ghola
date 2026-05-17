// v2 schema shape tests. Pinned BEFORE the schema.rs rewrite so we can
// watch them fail against the v1 schema (12 tables under `ghola`)
// and then go green once Task 1.3 installs the 5-table `semantic` schema.

#[cfg(any(test, feature = "pg_test"))]
#[pgrx::pg_schema]
mod tests {
    use pgrx::prelude::*;

    /// v2 has exactly five tables under the `semantic` schema, in alpha
    /// order: associations, co_activation_queue, contradiction_candidates,
    /// contradiction_queue, mnemes.
    #[pg_test]
    fn v2_schema_has_five_tables_only() {
        let tables: String = Spi::get_one(
            "SELECT array_agg(tablename ORDER BY tablename)::text \
             FROM pg_tables WHERE schemaname = 'semantic'",
        )
        .expect("query failed")
        .expect("null — schema `semantic` has no tables");
        assert_eq!(
            tables,
            "{associations,co_activation_queue,contradiction_candidates,contradiction_queue,mnemes}"
        );
    }

    /// v2 drops sub_mnemes + the cluster pathway entirely.
    #[pg_test]
    fn v2_has_no_sub_mnemes_no_clusters() {
        let count: i64 = Spi::get_one(
            "SELECT count(*)::bigint FROM pg_tables \
             WHERE tablename IN ('sub_mnemes','clusters','mneme_clusters')",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count, 0);
    }

    /// Semantic mnemes carry their contributor_user_ids uuid[] column so
    /// Pipeline B can preserve attribution across users during
    /// distillation.
    #[pg_test]
    fn mnemes_has_contributor_user_ids_column() {
        let exists: bool = Spi::get_one(
            "SELECT EXISTS (SELECT 1 FROM information_schema.columns \
             WHERE table_schema='semantic' AND table_name='mnemes' \
               AND column_name='contributor_user_ids')",
        )
        .expect("query failed")
        .expect("null");
        assert!(exists, "semantic.mnemes.contributor_user_ids must exist");
    }
}
