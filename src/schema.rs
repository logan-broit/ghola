// pg_recall::schema — Table, index, and constraint definitions
//
// Defines mnemes, associations, and co_activation_queue tables via
// pgrx extension_sql! macros. All objects live in the pg_recall schema.
//
// Owned by: create_extension_schema task

use pgrx::prelude::*;

// Stub: Schema SQL will be defined by the create_extension_schema task.
// Using extension_sql! to register table DDL in the extension install script.

extension_sql!(
    r#"
-- Stub: Tables, indexes, and constraints will be defined by create_extension_schema task.
-- This placeholder ensures the module compiles and the extension installs cleanly.
"#,
    name = "schema_stub",
);
