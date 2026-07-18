# Design: operator self-heal on replica repl auth failure (error 1045)

**Date:** 2026-07-18
**Status:** Approved (design)
**Repo:** ubc/mariadb-operator fork

## Problem

Replication replicas can get permanently stuck with **error 1045 (`Access denied for
user 'repl'`)** when the `repl` password diverges between the master's grant and the
replica's stored `master.info` (e.g. the repl-password secret is edited, or a manual
`ALTER USER` / partial reconcile happens). The replica's IO thread can never
authenticate, so the replica pod fails its readiness probe and the MariaDB goes
`StatefulSetNotReady` indefinitely.

The operator does **not** self-heal this:

- Replica configuration is **role-gated, not health-gated**. In
  `reconcileReplicationInPod` (`pkg/controller/replication/controller.go:263-267`), if a
  pod is already `role == Replica` the reconcile early-returns **without** re-issuing
  `CHANGE MASTER`. `ConfigureReplica` only runs via `forceReplicaConfiguration`, which is
  set only at init (`internal/controller/mariadb_controller_init.go:464`) and during
  switchover. So the master gets a new repl password (via
  `ConfigurePrimary` → `reconcileReplUserSql` → `ALTER USER`) but the replica never does.
- Replica auto-recovery does not cover it: `recoverableIOErrorCodes = {1236}` only (1045
  excluded), and its action is a heavy re-provision from backup (`bootstrapFrom`), not a
  lightweight credential re-sync.

Field incident (2026-07-18): 7 `hotcrp-*` DBs on the appcloud prod cluster were stuck on
1045 and had to be recovered by hand
(`STOP SLAVE; CHANGE MASTER TO MASTER_PASSWORD='<secret>'; START SLAVE`). See memory
note `hotcrp-repl-credential-desync`.

## Goal

When a replica's IO thread is failing authentication (`Last_IO_Errno == 1045`) and the
feature is opted in, the operator re-issues `CHANGE MASTER` with the current secret so the
replica reconnects — closing the role-gate gap.

## Non-goals

- No change to the backup-rebuild recovery path or `recoverableIOErrorCodes` (1045 stays
  out of it; auth is not a data-divergence problem).
- No secret watching / rotation workflow (the repl secret is static and unmanaged; out of
  scope).
- No behavior change for clusters that don't opt in.

## Design

### 1. API — `api/v1alpha1/mariadb_replication_types.go`

Add one optional bool to the existing `ReplicaRecovery` struct, working **independently**
of `Enabled`/`bootstrapFrom`:

```go
// ReconnectOnAuthError, when true, makes the operator re-issue CHANGE MASTER on a
// replica whose IO thread is failing authentication (error 1045) for longer than
// ErrorDurationThreshold, re-syncing the repl password from the secret. This works
// independently of Enabled/bootstrapFrom (it does not trigger a backup rebuild).
// It defaults to false.
// +optional
ReconnectOnAuthError bool `json:"reconnectOnAuthError,omitempty"`
```

Helper on `MariaDB` (mirroring existing `IsReplicaRecoveryEnabled`):

```go
func (m *MariaDB) IsReplicaReconnectOnAuthErrorEnabled() bool {
    repl := ptr.Deref(m.Spec.Replication, Replication{})
    rec := ptr.Deref(repl.Replica.Recovery, ReplicaRecovery{})
    return rec.ReconnectOnAuthError
}
```

Enabling on a DB is then a single field:

```yaml
spec:
  replication:
    replica:
      recovery:
        reconnectOnAuthError: true   # no enabled/bootstrapFrom needed
```

### 2. Detection — sustained 1045, reusing the existing threshold

Introduce the error code constant and a predicate. A replica **qualifies** when all hold:

1. `m.IsReplicaReconnectOnAuthErrorEnabled()` is true, and
2. its `ReplicaStatus.LastIOErrno == 1045`, and
3. the error has persisted past `recovery.ErrorDurationThreshold` (default 5m), computed
   from `LastErrorTransitionTime` — the same sustained-error mechanism `isRecoverableError`
   already uses.

The 5-minute debounce avoids reacting during a legitimate password-change window, where the
master and replica are briefly skewed by seconds.

```go
const replAuthErrorCode = 1045 // ER_ACCESS_DENIED_ERROR

// Pure predicate (unit-testable, no receiver): qualifies when the feature is enabled and
// this replica has a sustained 1045 IO error.
func replicaAuthResyncQualifies(mdb *MariaDB, status ReplicaStatus, threshold time.Duration, now time.Time) bool
```

Placed alongside the existing replica-status predicates (same file/pattern as
`isRecoverableError`), so status access and the threshold logic stay consistent.

A thin reconcile-time wrapper resolves the pod's `ReplicaStatus` (from
`mdb.Status.Replication.Replicas[pod]`), the effective `ErrorDurationThreshold`, and the
current time, then delegates to `replicaAuthResyncQualifies`:

```go
func (r *ReplicationReconciler) podNeedsAuthResync(mdb *MariaDB, pod string) bool
```

### 3. Action — reconcile-path hook

In `reconcileReplicationInPod` (`controller.go`), at the role-gate early-return for an
existing replica:

```go
if !opts.forceReplicaConfiguration {
    role, ok := replRoles[pod]
    if ok && role == mariadbv1alpha1.ReplicationRoleReplica {
        // NEW: if opted in and this replica has a sustained auth (1045) failure, do NOT
        // early-return; fall through to re-issue CHANGE MASTER with the current secret.
        if !r.podNeedsAuthResync(req.mariadb, pod) {
            return ctrl.Result{}, nil
        }
        // else: fall through to ConfigureReplica below
    }
}
...
// getReplicaOpts must return non-nil opts on this path (see note), then:
topology.ConfigureReplica(ctx, client, primaryPodIndex, replicaOpts...)
```

`ConfigureReplica` runs `STOP SLAVE; CHANGE MASTER TO … MASTER_PASSWORD=<current secret>;
START SLAVE`. Once the replica reconnects, `LastIOErrno → 0`, the predicate no longer
qualifies, and it stops firing — naturally self-limiting, no reconcile loop.

**Note on `getReplicaOpts`:** it currently returns `nil, nil` unless
`forceReplicaConfiguration`. The auth-resync path must produce the same replica opts as a
forced reconfiguration so `ConfigureReplica` uses the correct master/credentials. The
cleanest implementation reuses `WithForceReplicaConfiguration(true)` semantics for this
pod when it qualifies (either by threading a per-pod force flag, or by treating "qualifies
for auth-resync" as equivalent to force for the opts computation). Exact wiring is a
plan-level detail; the invariant is: qualifying replica → full `ConfigureReplica` with
real opts.

### 4. Scope & interactions

- **1045-only.** Not added to `recoverableIOErrorCodes`; the backup-rebuild path is
  unchanged.
- **Independent** of `recovery.enabled`/`bootstrapFrom` and of the recovery controller.
- **Switchover untouched** — those paths already force `ConfigureReplica`.
- **No effect on healthy replicas** — only fires when replication is already down on auth.

## Code touch points

- `api/v1alpha1/mariadb_replication_types.go` — new field + helper.
- `api/v1alpha1/zz_generated.deepcopy.go` — regenerated (field is a scalar; likely no
  manual change, regenerate to be safe).
- `internal/controller/mariadb_controller_replica_recovery.go` (or a small sibling file in
  the replication controller) — the `replAuthErrorCode` const + `replicaNeedsAuthResync`
  predicate.
- `pkg/controller/replication/controller.go` — the reconcile-path hook + opts wiring.
- CRDs + docs: `make gen` regenerates `config/crd/**`, `deploy/**` CRDs, and
  `docs/api_reference.md`.

## Testing

Table-driven unit tests (following `pkg/controller/replication/switchover_test.go` and
`internal/controller/mariadb_controller_replica_recovery_test.go`):

- `replicaAuthResyncQualifies` predicate:
  - flag off + 1045 sustained → false
  - flag on + 1045 sustained past threshold → true
  - flag on + 1045 but sub-threshold → false
  - flag on + non-1045 errno (e.g. 1236, 0) → false
  - flag on + no replica status / errno nil → false
- Reconcile behavior: a role=Replica pod that qualifies triggers `ConfigureReplica`;
  one that doesn't still early-returns. (Prefer testing the predicate + a thin seam so the
  reconcile assertion doesn't need envtest; envtest integration only if a cheap seam isn't
  possible.)

## Rollout

1. Implement + unit tests green (`go test ./...`), `go vet`.
2. `make gen` (CRDs, deepcopy, `api_reference.md`); commit generated artifacts so the
   Artifacts CI check passes.
3. Push branch → PR; CI (build, unit, integration, helm) green.
4. Merge to `main`; CI publishes `ghcr.io/ubc/mariadb-operator:<sha>` and the fork charts.
5. `helm upgrade` the appcloud operator to the new image (see memory
   `mariadb-operator-appcloud-deploy`).
6. Set `spec.replication.replica.recovery.reconnectOnAuthError: true` on the 7 hotcrp DBs
   (in `k8s-config`), so a future desync self-heals.

## Risks

- **Reconfigure churn** if a replica is genuinely mis-credentialed at the *secret* level
  (secret itself wrong): the operator would re-`CHANGE MASTER` every reconcile-after-threshold
  and keep failing 1045. Mitigation: the 5m threshold rate-limits it; it does no harm beyond
  log noise and repeated STOP/START SLAVE on an already-broken replica. Acceptable.
- **Fork divergence** from upstream. Mitigation: the change is small, opt-in, and
  upstreamable; propose upstream after it soaks.
