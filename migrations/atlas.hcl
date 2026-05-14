// atlas.hcl — Atlas (versioned mode) configuration for the Neksur SaaS
// migration pipeline. D-0.5.17 + D-0.5.18 lock Atlas as the migration
// tool; `cmd/migrate` wraps `atlas migrate apply` for the multi-tenant
// rollout (public-tier once, then each `tenant_<uuid>` schema).
//
// Two env blocks:
//   * `public` — applies to the shared `public` schema (tenants registry,
//     billing stub, audit log). Used by the first invocation of the
//     tenant-loop wrapper and by the BLOCKING Task 4 verification.
//   * `tenant` — applies to per-tenant `tenant_<uuid>` schemas. The
//     tenant-loop wrapper sets `search_path=tenant_<uuid>,public` via
//     the URL so Atlas applies migrations into the tenant schema while
//     writing revision history to the shared `public.atlas_schema_revisions`
//     table (RESEARCH §Pitfall 9 — `revisions_schema = "public"`).
//
// Both env blocks declare `exclude = ["ag_catalog.*", "tenant_*"]` so
// Atlas never proposes diffs against the AGE catalog (Pitfall 3) or
// against the dynamic per-tenant schemas (which the tenant-loop iterates
// explicitly, NOT via Atlas auto-discovery).
//
// `diff { skip { drop_schema, drop_table } }` is belt-and-suspenders —
// even if Atlas's exclude list somehow misses a tenant schema, the diff
// will refuse to emit DROP statements against it.

// `variable "url"` declares an input the env blocks consume; callers
// override via --url on the CLI (cmd/migrate + Make targets do this).
variable "url" {
  type    = string
  default = ""
}

env "public" {
  // The URL is injected via --url on the CLI (Atlas's --url flag
  // overrides the env block's url). The empty default allows
  // `atlas migrate validate` to run without a live target.
  url = var.url

  // `docker://postgres/16/dev` spins a throwaway Postgres 16 container
  // Atlas uses to validate migration HCL — it does NOT touch the real
  // DB. AGE is not required here because we exclude ag_catalog.* below.
  dev = "docker://postgres/16/dev"

  // Phase 0 files use the Flyway naming convention V<seq>__<slug>.sql
  // (D-0.5.18 preserves this for continuity); Atlas needs the URL
  // form `file://path?format=flyway` because the migration{} block in
  // atlas.hcl does NOT have a `format` field — format is encoded as a
  // URL query parameter on the dir URL itself (Atlas v1.2.1 convention).
  migration {
    dir     = "file://migrations/postgres?format=flyway"
    exclude = ["ag_catalog.*", "tenant_*"]
  }

  diff {
    skip {
      drop_schema = true
      drop_table  = true
    }
  }

  // All Atlas revision rows land in public.atlas_schema_revisions so
  // a single cross-tenant audit query suffices.
  revisions_schema = "public"
}

env "tenant" {
  // cmd/migrate overrides --url to inject the per-tenant search_path.
  url = var.url

  dev = "docker://postgres/16/dev"

  migration {
    dir     = "file://migrations/postgres?format=flyway"
    exclude = ["ag_catalog.*", "tenant_*"]
  }

  diff {
    skip {
      drop_schema = true
      drop_table  = true
    }
  }

  // CRITICAL: all tenants share public.atlas_schema_revisions so the
  // cross-tenant migration audit query works.
  revisions_schema = "public"
}
