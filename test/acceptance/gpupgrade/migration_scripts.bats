#! /usr/bin/env bats
# Copyright (c) 2017-2023 VMware, Inc. or its affiliates
# SPDX-License-Identifier: Apache-2.0

load ../helpers/helpers
load ../helpers/teardown_helpers

SEED_DIR=$BATS_TEST_DIRNAME/../../../data-migration-scripts

setup() {
    # FIXME: We should support testing the migration scripts.
    if is_GPDB6 "$GPHOME_SOURCE"; then
        skip "FIXME: migration scripts are not yet supported for 6-to-6 and 6-to-7 upgrades"
    fi

    skip_if_no_gpdb

    STATE_DIR=$(mktemp -d /tmp/gpupgrade.XXXXXX)
    register_teardown archive_state_dir "$STATE_DIR"

    export GPUPGRADE_HOME="${STATE_DIR}/gpupgrade"
    gpupgrade kill-services

    backup_source_cluster "$STATE_DIR"/backup

    PSQL="$GPHOME_SOURCE/bin/psql -X --no-align --tuples-only"

    export VERSION_SEED_DIR=6-to-7-seed-scripts
    if is_GPDB5 "$GPHOME_SOURCE"; then
        export VERSION_SEED_DIR=5-to-6-seed-scripts
    fi

    $PSQL -v ON_ERROR_STOP=1 -d postgres -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/setup_nonupgradable_objects.sql
}

teardown() {
    $PSQL -v ON_ERROR_STOP=1 -d postgres -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/teardown_nonupgradable_objects.sql

    # XXX Beware, BATS_TEST_SKIPPED is not a documented export.
    if [ -n "${BATS_TEST_SKIPPED}" ]; then
        return
    fi

    gpupgrade kill-services

    run_teardowns
}

@test "migration scripts generate sql to modify non-upgradeable objects and fix pg_upgrade check errors" {
    # This syntax in create_nonupgradable_objects.sql is not completely
    # compatible with 6X. ON_ERROR_STOP is disabled until this incompatibility
    # is resolved.
    PGOPTIONS='--client-min-messages=warning' $PSQL -v ON_ERROR_STOP=0 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/create_nonupgradable_objects.sql
    run gpupgrade initialize \
        --source-gphome="$GPHOME_SOURCE" \
        --target-gphome="$GPHOME_TARGET" \
        --source-master-port="${PGPORT}" \
        --temp-port-range 6020-6040 \
        --disk-free-ratio 0 \
        --non-interactive \
        --verbose
    echo "$output"
    [ "$status" -ne 0 ] || fail "expected initialize to fail due to pg_upgrade check"

    egrep "\"check_upgrade\": \"failed\"" $GPUPGRADE_HOME/substeps.json
    egrep "^Checking.*fatal$" ~/gpAdminLogs/gpupgrade/pg_upgrade/p-1/pg_upgrade_internal.log

    PGOPTIONS='--client-min-messages=warning' $PSQL -v ON_ERROR_STOP=1 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/drop_unfixable_objects.sql

    root_child_indexes_before=$(get_indexes "$GPHOME_SOURCE")
    tsquery_datatype_objects_before=$(get_tsquery_datatypes "$GPHOME_SOURCE")
    name_datatype_objects_before=$(get_name_datatypes "$GPHOME_SOURCE")
    fk_constraints_before=$(get_fk_constraints "$GPHOME_SOURCE")
    primary_unique_constraints_before=$(get_primary_unique_constraints "$GPHOME_SOURCE")
    partition_owners_before=$(get_partition_owners "$GPHOME_SOURCE")
    partition_constraints_before=$(get_partition_constraints "$GPHOME_SOURCE")
    partition_defaults_before=$(get_partition_defaults "$GPHOME_SOURCE")
    view_owners_before=$(get_view_owners "$GPHOME_SOURCE")

    MIGRATION_DIR=`mktemp -d /tmp/migration.XXXXXX`
    gpupgrade generate --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --seed-dir "$SEED_DIR" --output-dir "$MIGRATION_DIR"
    gpupgrade apply    --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase initialize

    gpupgrade initialize \
        --source-gphome="$GPHOME_SOURCE" \
        --target-gphome="$GPHOME_TARGET" \
        --source-master-port="${PGPORT}" \
        --temp-port-range 6020-6040 \
        --disk-free-ratio 0 \
        --non-interactive \
        --verbose
    gpupgrade execute --non-interactive --verbose
    gpupgrade finalize --non-interactive --verbose

    gpupgrade apply --non-interactive --gphome "$GPHOME_TARGET" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase finalize

    # migration scripts should create the indexes on the target cluster
    root_child_indexes_after=$(get_indexes "$GPHOME_TARGET")
    tsquery_datatype_objects_after=$(get_tsquery_datatypes "$GPHOME_TARGET")
    name_datatype_objects_after=$(get_name_datatypes "$GPHOME_TARGET")
    fk_constraints_after=$(get_fk_constraints "$GPHOME_TARGET")
    primary_unique_constraints_after=$(get_primary_unique_constraints "$GPHOME_TARGET")
    partition_owners_after=$(get_partition_owners "$GPHOME_TARGET")
    partition_constraints_after=$(get_partition_constraints "$GPHOME_TARGET")
    partition_defaults_after=$(get_partition_defaults "$GPHOME_TARGET")
    view_owners_after=$(get_view_owners "$GPHOME_TARGET")

    # expect the index and tsquery datatype information to be same after the upgrade
    diff -U3 <(echo "$root_child_indexes_before") <(echo "$root_child_indexes_after")
    diff -U3 <(echo "$tsquery_datatype_objects_before") <(echo "$tsquery_datatype_objects_after")
    diff -U3 <(echo "$name_datatype_objects_before") <(echo "$name_datatype_objects_after")
    diff -U3 <(echo "$fk_constraints_before") <(echo "$fk_constraints_after")
    diff -U3 <(echo "$primary_unique_constraints_before") <(echo "$primary_unique_constraints_after")
    diff -U3 <(echo "$partition_owners_before") <(echo "$partition_owners_after")
    diff -U3 <(echo "$partition_constraints_before") <(echo "$partition_constraints_after")
    diff -U3 <(echo "$partition_defaults_before") <(echo "$partition_defaults_after")
    diff -U3 <(echo "$view_owners_before") <(echo "$view_owners_after")
}

@test "recreate scripts restore both unfixable and non-upgradeable objects when reverting after initialize" {
    # This syntax in create_nonupgradable_objects.sql is not completely
    # compatible with 6X. ON_ERROR_STOP is disabled until this incompatibility
    # is resolved.
    $PSQL -v ON_ERROR_STOP=0 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/create_nonupgradable_objects.sql

    root_child_indexes_before=$(get_indexes "$GPHOME_SOURCE")
    tsquery_datatype_objects_before=$(get_tsquery_datatypes "$GPHOME_SOURCE")
    name_datatype_objects_before=$(get_name_datatypes "$GPHOME_SOURCE")
    fk_constraints_before=$(get_fk_constraints "$GPHOME_SOURCE")
    primary_unique_constraints_before=$(get_primary_unique_constraints "$GPHOME_SOURCE")
    view_owners_before=$(get_view_owners "$GPHOME_SOURCE")

    # Ignore the test tables that break the diff for now.
    EXCLUSIONS+="-T testschema.heterogeneous_ml_partition_table "

    MIGRATION_DIR=`mktemp -d /tmp/migration.XXXXXX`
    register_teardown rm -r "$MIGRATION_DIR"

    gpupgrade generate --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --seed-dir "$SEED_DIR" --output-dir "$MIGRATION_DIR"
    gpupgrade apply    --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase initialize

    run gpupgrade initialize \
        --source-gphome="$GPHOME_SOURCE" \
        --target-gphome="$GPHOME_TARGET" \
        --source-master-port="${PGPORT}" \
        --temp-port-range 6020-6040 \
        --disk-free-ratio 0 \
        --non-interactive \
        --verbose
    [ "$status" -ne 0 ] || fail "expected initialize to fail due to unfixable"

    gpupgrade revert --non-interactive --verbose

    gpupgrade apply --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase revert

    # migration scripts should recreate indexes on the source cluster on revert
    root_child_indexes_after=$(get_indexes "$GPHOME_SOURCE")
    tsquery_datatype_objects_after=$(get_tsquery_datatypes "$GPHOME_SOURCE")
    name_datatype_objects_after=$(get_name_datatypes "$GPHOME_SOURCE")
    fk_constraints_after=$(get_fk_constraints "$GPHOME_SOURCE")
    primary_unique_constraints_after=$(get_primary_unique_constraints "$GPHOME_SOURCE")
    view_owners_after=$(get_view_owners "$GPHOME_SOURCE")

    # expect the index and tsquery datatype information to be same after the upgrade
    diff -U3 <(echo "$root_child_indexes_before") <(echo "$root_child_indexes_after")
    diff -U3 <(echo "$tsquery_datatype_objects_before") <(echo "$tsquery_datatype_objects_after")
    diff -U3 <(echo "$name_datatype_objects_before") <(echo "$name_datatype_objects_after")
    diff -U3 <(echo "$fk_constraints_before") <(echo "$fk_constraints_after")
    diff -U3 <(echo "$primary_unique_constraints_before") <(echo "$primary_unique_constraints_after")
    diff -U3 <(echo "$view_owners_before") <(echo "$view_owners_after")
}

@test "recreate scripts restore non-upgradeable objects when reverting after execute" {
    # This syntax in create_nonupgradable_objects.sql is not completely
    # compatible with 6X. ON_ERROR_STOP is disabled until this incompatibility
    # is resolved.
    $PSQL -v ON_ERROR_STOP=0 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/create_nonupgradable_objects.sql
    $PSQL -v ON_ERROR_STOP=1 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/drop_unfixable_objects.sql

    root_child_indexes_before=$(get_indexes "$GPHOME_SOURCE")
    tsquery_datatype_objects_before=$(get_tsquery_datatypes "$GPHOME_SOURCE")
    name_datatype_objects_before=$(get_name_datatypes "$GPHOME_SOURCE")
    fk_constraints_before=$(get_fk_constraints "$GPHOME_SOURCE")
    primary_unique_constraints_before=$(get_primary_unique_constraints "$GPHOME_SOURCE")
    view_owners_before=$(get_view_owners "$GPHOME_SOURCE")

    # Ignore the test tables that break the diff for now.
    EXCLUSIONS+="-T testschema.heterogeneous_ml_partition_table "

    MIGRATION_DIR=`mktemp -d /tmp/migration.XXXXXX`
    register_teardown rm -r "$MIGRATION_DIR"

    gpupgrade generate --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --seed-dir "$SEED_DIR" --output-dir "$MIGRATION_DIR"
    gpupgrade apply    --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase initialize

    gpupgrade initialize \
        --source-gphome="$GPHOME_SOURCE" \
        --target-gphome="$GPHOME_TARGET" \
        --source-master-port="${PGPORT}" \
        --temp-port-range 6020-6040 \
        --disk-free-ratio 0 \
        --non-interactive \
        --verbose
    gpupgrade execute --non-interactive --verbose
    gpupgrade revert --non-interactive --verbose

    gpupgrade apply --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase revert

    # migration scripts should create the indexes on the target cluster
    root_child_indexes_after=$(get_indexes "$GPHOME_SOURCE")
    tsquery_datatype_objects_after=$(get_tsquery_datatypes "$GPHOME_SOURCE")
    name_datatype_objects_after=$(get_name_datatypes "$GPHOME_SOURCE")
    fk_constraints_after=$(get_fk_constraints "$GPHOME_SOURCE")
    primary_unique_constraints_after=$(get_primary_unique_constraints "$GPHOME_SOURCE")
    view_owners_after=$(get_view_owners "$GPHOME_TARGET")

    # expect the index and tsquery datatype information to be same after the upgrade
    diff -U3 <(echo "$root_child_indexes_before") <(echo "$root_child_indexes_after")
    diff -U3 <(echo "$tsquery_datatype_objects_before") <(echo "$tsquery_datatype_objects_after")
    diff -U3 <(echo "$name_datatype_objects_before") <(echo "$name_datatype_objects_after")
    diff -U3 <(echo "$fk_constraints_before") <(echo "$fk_constraints_after")
    diff -U3 <(echo "$primary_unique_constraints_before") <(echo "$primary_unique_constraints_after")
    diff -U3 <(echo "$view_owners_before") <(echo "$view_owners_after")
}

@test "migration scripts ignore .psqlrc files" {
    # 5X doesn't support the PSQLRC envvar we need to avoid destroying the dev
    # environment.
    if is_GPDB5 "$GPHOME_SOURCE"; then
        skip "GPDB 5 does not support alternative PSQLRC locations"
    fi

    # This syntax in create_nonupgradable_objects.sql is not completely
    # compatible with 6X. ON_ERROR_STOP is disabled until this incompatibility
    # is resolved.
    $PSQL -v ON_ERROR_STOP=0 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/create_nonupgradable_objects.sql

    MIGRATION_DIR=$(mktemp -d /tmp/migration.XXXXXX)
    register_teardown rm -r "$MIGRATION_DIR"

    # Set up psqlrc to kill any psql processes as soon as they're started.
    export PSQLRC="$MIGRATION_DIR"/psqlrc
    printf '\! kill $PPID\n' > "$PSQLRC"

    gpupgrade generate --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --seed-dir "$SEED_DIR" --output-dir "$MIGRATION_DIR"
    $PSQL -v ON_ERROR_STOP=1 -d testdb -f "${SEED_DIR}"/"${VERSION_SEED_DIR}"/test/drop_unfixable_objects.sql
    gpupgrade apply    --non-interactive --gphome "$GPHOME_SOURCE" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase initialize

    gpupgrade initialize \
        --source-gphome="$GPHOME_SOURCE" \
        --target-gphome="$GPHOME_TARGET" \
        --source-master-port="${PGPORT}" \
        --temp-port-range 6020-6040 \
        --disk-free-ratio 0 \
        --non-interactive \
        --verbose
    gpupgrade revert --non-interactive --verbose

    gpupgrade apply --non-interactive --gphome "$GPHOME_TARGET" --port "$PGPORT" --input-dir "$MIGRATION_DIR" --phase revert
}

get_indexes() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
         SELECT indrelid::regclass, unnest(indkey)
         FROM pg_index pi
         JOIN pg_partition pp ON pi.indrelid=pp.parrelid
         JOIN pg_class pc ON pc.oid=pp.parrelid
         ORDER by 1,2;
        "
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
        SELECT indrelid::regclass, unnest(indkey)
        FROM pg_index pi
        JOIN pg_partition_rule pp ON pi.indrelid=pp.parchildrelid
        JOIN pg_class pc ON pc.oid=pp.parchildrelid
        WHERE pc.relhassubclass='f'
        ORDER by 1,2;
    "
}

get_tsquery_datatypes() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
        SELECT n.nspname, c.relname, a.attname
        FROM pg_catalog.pg_class c,
             pg_catalog.pg_namespace n,
            pg_catalog.pg_attribute a
        WHERE c.relkind = 'r'
        AND c.oid = a.attrelid
        AND NOT a.attisdropped
        AND a.atttypid = 'pg_catalog.tsquery'::pg_catalog.regtype
        AND c.relnamespace = n.oid
        AND n.nspname !~ '^pg_temp_'
        AND n.nspname !~ '^pg_toast_temp_'
        AND n.nspname NOT IN ('pg_catalog',
                                'information_schema')
        AND c.oid NOT IN
            (SELECT DISTINCT parchildrelid
            FROM pg_catalog.pg_partition_rule)
        ORDER BY 1,2,3;
        "
}

get_name_datatypes() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
        SELECT n.nspname, c.relname, a.attname
        FROM pg_catalog.pg_class c,
             pg_catalog.pg_namespace n,
            pg_catalog.pg_attribute a
        WHERE c.relkind = 'r'
        AND c.oid = a.attrelid
        AND NOT a.attisdropped
        AND a.atttypid = 'pg_catalog.name'::pg_catalog.regtype
        AND c.relnamespace = n.oid
        AND n.nspname !~ '^pg_temp_'
        AND n.nspname !~ '^pg_toast_temp_'
        AND n.nspname NOT IN ('pg_catalog',
                                'information_schema')
        AND c.oid NOT IN
            (SELECT DISTINCT parchildrelid
            FROM pg_catalog.pg_partition_rule)
        ORDER BY 1,2,3;
        "
}

get_fk_constraints() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
        SELECT nspname, relname, conname
        FROM pg_constraint cc
            JOIN
            (
            SELECT DISTINCT
                c.oid,
                n.nspname,
                c.relname
            FROM
                pg_catalog.pg_partition p
            JOIN
                pg_catalog.pg_class c
                ON (p.parrelid = c.oid)
            JOIN
                pg_catalog.pg_namespace n
                ON (n.oid = c.relnamespace)
            ) AS sub
            ON sub.oid = cc.conrelid
        WHERE
            cc.contype = 'f'
        ORDER BY 1,2,3;
        "
}

get_primary_unique_constraints() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
    WITH CTE as
    (
        SELECT oid, *
        FROM pg_class
        WHERE
            oid NOT IN
            (
                SELECT DISTINCT
                    parchildrelid
                FROM
                    pg_partition_rule
            )
    )
    SELECT
        n.nspname, cc.relname, conname
    FROM
        pg_constraint con
        JOIN
        pg_depend dep
        ON (refclassid, classid, objsubid) =
        (
           'pg_constraint'::regclass,
           'pg_class'::regclass,
           0
        )
        AND refobjid = con.oid
        AND deptype = 'i'
        -- Note that 'x' is an option for contype in GPDB6, and not in GPDB5
        -- It is included in this query to make it compatible for both.
        AND contype IN ('u', 'p', 'x')
        JOIN
        CTE c
        ON objid = c.oid
        AND relkind = 'i'
        JOIN
        CTE cc
        ON cc.oid = con.conrelid
        JOIN
        pg_namespace n
        ON (n.oid = cc.relnamespace)
    ORDER BY 1,2,3;
    "
}

get_partition_owners() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
    SELECT c.relname, pg_catalog.pg_get_userbyid(c.relowner)
    FROM pg_partition_rule pr
        JOIN pg_class c ON c.oid = pr.parchildrelid
    UNION
    SELECT c.relname, pg_catalog.pg_get_userbyid(c.relowner)
    FROM pg_partition p
        JOIN pg_class c ON c.oid = p.parrelid
    ORDER BY 1,2;
    "
}

get_partition_constraints() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
    SELECT c.relname, con.conname
    FROM pg_partition_rule pr
        JOIN pg_class c ON c.oid = pr.parchildrelid
        JOIN pg_constraint con ON con.conrelid = c.oid
    WHERE c.relname NOT LIKE 'table_with_primary_constraint%'
        AND c.relname NOT LIKE 'table_with_unique_constraint%'
    UNION
    SELECT c.relname, con.conname
    FROM pg_partition p
        JOIN pg_class c ON c.oid = p.parrelid
        JOIN pg_constraint con ON con.conrelid = c.oid
    WHERE c.relname NOT LIKE 'table_with_primary_constraint%'
        AND c.relname NOT LIKE 'table_with_unique_constraint%'
    ORDER BY 1,2;
    "
}

get_partition_defaults() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
    SELECT c.relname, att.attname, ad.adnum, ad.adsrc
    FROM pg_partition_rule pr
        JOIN pg_class c ON c.oid = pr.parchildrelid
        JOIN pg_attrdef ad ON ad.adrelid = pr.parchildrelid
        JOIN pg_attribute att ON att.attrelid = c.oid and att.attnum = ad.adnum
    UNION
    SELECT c.relname, att.attname, ad.adnum, ad.adsrc
    FROM pg_partition p
        JOIN pg_class c ON c.oid = p.parrelid
        JOIN pg_attrdef ad ON ad.adrelid = p.parrelid
        JOIN pg_attribute att ON att.attrelid = c.oid and att.attnum = ad.adnum
    ORDER BY 1,2,3,4;
    "
}

get_view_owners() {
    local gphome=$1
    $gphome/bin/psql -v ON_ERROR_STOP=1 -d testdb -p "$PGPORT" -Atc "
    SELECT schemaname, viewname, viewowner
    FROM pg_views
    WHERE schemaname NOT IN ('pg_catalog', 'information_schema', 'gp_toolkit')
    ORDER BY 1,2,3;
    "
}
