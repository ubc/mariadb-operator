# Replica Reconnect-On-Auth-Error Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in operator behavior that re-issues `CHANGE MASTER` on a replication replica stuck on repl auth error 1045, re-syncing its master password from the secret.

**Architecture:** A new bool field `spec.replication.replica.recovery.reconnectOnAuthError` gates the behavior. A pure predicate detects a replica with a sustained (past `errorDurationThreshold`) `LastIOErrno == 1045`. A hook in `ReconcileReplicationInPod` — at the existing role-gate early-return for an already-`Replica` pod — calls `ConfigureReplica(..., WithResetMaster(false))` when the replica qualifies, instead of returning early. Independent of the backup-rebuild recovery path.

**Tech Stack:** Go (module `github.com/mariadb-operator/mariadb-operator/v26`), controller-runtime, Kubebuilder codegen (`controller-gen`, `kustomize`, `crd-ref-docs`), `go test`.

## Global Constraints

- Go module path: `github.com/mariadb-operator/mariadb-operator/v26` — import api as `mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"`.
- Field defaults to `false`; behavior is fully opt-in and MUST NOT change behavior for clusters that don't set it.
- Works **independently** of `recovery.Enabled`/`bootstrapFrom` (no backup rebuild).
- Trigger is **1045 only**. Do NOT add 1045 to `recoverableIOErrorCodes` (that path stays backup-only).
- Reuse `recovery.ErrorDurationThreshold` (default `5 * time.Minute`) as the sustained-error debounce.
- The auth-resync `ConfigureReplica` call MUST pass `WithResetMaster(false)` (a lightweight re-CHANGE-MASTER; no `RESET MASTER` on the replica).

---

## File Structure

- `api/v1alpha1/mariadb_replication_types.go` (modify) — new `ReconnectOnAuthError` field on `ReplicaRecovery` (~line 150) + `IsReplicaReconnectOnAuthErrorEnabled()` helper (near `IsReplicaRecoveryEnabled` ~line 402).
- `pkg/controller/replication/auth_resync.go` (create) — `replAuthErrorCode` const, pure predicate `replicaAuthResyncQualifies`, reconciler method `podNeedsAuthResync`. One responsibility: decide whether a replica pod needs an auth re-sync.
- `pkg/controller/replication/auth_resync_test.go` (create) — table-driven unit tests for the predicate + wrapper (plain `go test`, no envtest).
- `pkg/controller/replication/controller.go` (modify) — the role-gate hook in `ReconcileReplicationInPod` (~line 263-268).
- Generated (regenerate, do not hand-edit): `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/k8s.mariadb.com_mariadbs.yaml`, `deploy/charts/mariadb-operator-crds/templates/crds.yaml`, `deploy/crds/crds.yaml`, `docs/api_reference.md`.

---

### Task 1: API field, helper, detection predicate + wrapper (with unit tests)

**Files:**
- Modify: `api/v1alpha1/mariadb_replication_types.go` (add field ~line 150; add helper ~line 409)
- Create: `pkg/controller/replication/auth_resync.go`
- Test: `pkg/controller/replication/auth_resync_test.go`

**Interfaces:**
- Consumes: `mariadbv1alpha1.MariaDB`, `mariadbv1alpha1.ReplicaStatus` (embeds `ReplicaStatusVars{ LastIOErrno *int, LastSQLErrno *int }`, plus `LastErrorTransitionTime metav1.Time`), `mariadbv1alpha1.ReplicationStatus{ Replicas map[string]ReplicaStatus }`, `mariadbv1alpha1.ReplicaRecovery{ Enabled bool, ErrorDurationThreshold *metav1.Duration }`.
- Produces:
  - `func (m *mariadbv1alpha1.MariaDB) IsReplicaReconnectOnAuthErrorEnabled() bool`
  - `const replAuthErrorCode = 1045` (package `replication`)
  - `func replicaAuthResyncQualifies(mdb *mariadbv1alpha1.MariaDB, status mariadbv1alpha1.ReplicaStatus) bool`
  - `func (r *ReplicationReconciler) podNeedsAuthResync(mdb *mariadbv1alpha1.MariaDB, pod string) bool`

- [ ] **Step 1: Write the failing tests**

Create `pkg/controller/replication/auth_resync_test.go`:

```go
package replication

import (
	"testing"
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// mdbWithReconnect builds a replication MariaDB with the reconnectOnAuthError flag set.
// NOTE: Replica lives on the embedded ReplicationSpec, so it must be set through it —
// `Replication{Replica: ...}` does not compile (promoted field in a composite literal).
func mdbWithReconnect(reconnect bool) *mariadbv1alpha1.MariaDB {
	return &mariadbv1alpha1.MariaDB{
		Spec: mariadbv1alpha1.MariaDBSpec{
			Replication: &mariadbv1alpha1.Replication{
				Enabled: true,
				ReplicationSpec: mariadbv1alpha1.ReplicationSpec{
					Replica: mariadbv1alpha1.ReplicaReplication{
						ReplicaRecovery: &mariadbv1alpha1.ReplicaRecovery{
							ReconnectOnAuthError: reconnect,
						},
					},
				},
			},
		},
	}
}

func statusWithIOErrno(errno int, transition time.Time) mariadbv1alpha1.ReplicaStatus {
	return mariadbv1alpha1.ReplicaStatus{
		ReplicaStatusVars: mariadbv1alpha1.ReplicaStatusVars{
			LastIOErrno: ptr.To(errno),
		},
		LastErrorTransitionTime: metav1.NewTime(transition),
	}
}

func TestReplicaAuthResyncQualifies(t *testing.T) {
	sustained := time.Now().Add(-10 * time.Minute) // older than the 5m default threshold
	fresh := time.Now()

	tests := []struct {
		name   string
		mdb    *mariadbv1alpha1.MariaDB
		status mariadbv1alpha1.ReplicaStatus
		want   bool
	}{
		{
			name:   "flag off, sustained 1045 -> false",
			mdb:    mdbWithReconnect(false),
			status: statusWithIOErrno(1045, sustained),
			want:   false,
		},
		{
			name:   "flag on, sustained 1045 -> true",
			mdb:    mdbWithReconnect(true),
			status: statusWithIOErrno(1045, sustained),
			want:   true,
		},
		{
			name:   "flag on, fresh 1045 (sub-threshold) -> false",
			mdb:    mdbWithReconnect(true),
			status: statusWithIOErrno(1045, fresh),
			want:   false,
		},
		{
			name:   "flag on, sustained 1236 (not auth) -> false",
			mdb:    mdbWithReconnect(true),
			status: statusWithIOErrno(1236, sustained),
			want:   false,
		},
		{
			name:   "flag on, no IO errno -> false",
			mdb:    mdbWithReconnect(true),
			status: mariadbv1alpha1.ReplicaStatus{LastErrorTransitionTime: metav1.NewTime(sustained)},
			want:   false,
		},
		{
			name:   "flag on, 1045 but zero transition time -> false",
			mdb:    mdbWithReconnect(true),
			status: statusWithIOErrno(1045, time.Time{}),
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replicaAuthResyncQualifies(tt.mdb, tt.status); got != tt.want {
				t.Fatalf("replicaAuthResyncQualifies() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodNeedsAuthResync(t *testing.T) {
	sustained := time.Now().Add(-10 * time.Minute)
	mdb := mdbWithReconnect(true)
	mdb.Status.Replication = &mariadbv1alpha1.ReplicationStatus{
		Replicas: map[string]mariadbv1alpha1.ReplicaStatus{
			"mdb-1": statusWithIOErrno(1045, sustained),
		},
	}
	r := &ReplicationReconciler{}

	if !r.podNeedsAuthResync(mdb, "mdb-1") {
		t.Fatalf("expected pod mdb-1 to need auth resync")
	}
	if r.podNeedsAuthResync(mdb, "mdb-0") { // pod absent from status map
		t.Fatalf("expected pod mdb-0 (absent) to NOT need auth resync")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail (build failure — symbols undefined)**

Run: `go test ./pkg/controller/replication/ -run 'AuthResync|PodNeedsAuthResync' 2>&1 | head`
Expected: build FAILS — `undefined: replicaAuthResyncQualifies`, `undefined: ... ReconnectOnAuthError`, `r.podNeedsAuthResync undefined`.

- [ ] **Step 3a: Add the API field**

In `api/v1alpha1/mariadb_replication_types.go`, inside the `ReplicaRecovery` struct (immediately after the `MinHealthyDuration` field, before the closing `}` at ~line 150), add:

```go
	// ReconnectOnAuthError, when true, makes the operator re-issue CHANGE MASTER on a
	// replica whose IO thread is failing authentication (error 1045) for longer than
	// ErrorDurationThreshold, re-syncing the repl password from the secret. This works
	// independently of Enabled and does not trigger a backup rebuild.
	// It defaults to false.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec
	ReconnectOnAuthError bool `json:"reconnectOnAuthError,omitempty"`
```

- [ ] **Step 3b: Add the API helper**

In the same file, immediately after the `IsReplicaRecoveryEnabled` function (ends ~line 409), add:

```go
// IsReplicaReconnectOnAuthErrorEnabled indicates whether the operator should re-issue
// CHANGE MASTER on a replica stuck on a repl auth error (1045).
func (m *MariaDB) IsReplicaReconnectOnAuthErrorEnabled() bool {
	if !m.IsReplicationEnabled() {
		return false
	}
	replication := ptr.Deref(m.Spec.Replication, Replication{})
	recovery := ptr.Deref(replication.Replica.ReplicaRecovery, ReplicaRecovery{})
	return recovery.ReconnectOnAuthError
}
```

- [ ] **Step 3c: Create the predicate + wrapper**

Create `pkg/controller/replication/auth_resync.go`:

```go
package replication

import (
	"time"

	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// replAuthErrorCode is ER_ACCESS_DENIED_ERROR (1045): the replica's IO thread cannot
// authenticate to the master. Unlike replication-state errors, this is recoverable by
// re-issuing CHANGE MASTER with the current secret password (no backup rebuild).
const replAuthErrorCode = 1045

// replicaAuthResyncQualifies reports whether the given replica should have its master
// credentials re-synced: the feature is opted in, the replica's IO thread is failing with
// error 1045, and that error has persisted past recovery.ErrorDurationThreshold (default
// 5m). The threshold debounces legitimate password-change windows.
func replicaAuthResyncQualifies(mdb *mariadbv1alpha1.MariaDB, status mariadbv1alpha1.ReplicaStatus) bool {
	if !mdb.IsReplicaReconnectOnAuthErrorEnabled() {
		return false
	}
	if ptr.Deref(status.LastIOErrno, 0) != replAuthErrorCode {
		return false
	}
	if status.LastErrorTransitionTime.IsZero() {
		return false
	}
	replication := ptr.Deref(mdb.Spec.Replication, mariadbv1alpha1.Replication{})
	recovery := ptr.Deref(replication.Replica.ReplicaRecovery, mariadbv1alpha1.ReplicaRecovery{})
	threshold := ptr.Deref(recovery.ErrorDurationThreshold, metav1.Duration{Duration: 5 * time.Minute})
	return time.Since(status.LastErrorTransitionTime.Time) > threshold.Duration
}

// podNeedsAuthResync resolves the replica status for pod and delegates to
// replicaAuthResyncQualifies. Returns false if the pod has no recorded replica status.
func (r *ReplicationReconciler) podNeedsAuthResync(mdb *mariadbv1alpha1.MariaDB, pod string) bool {
	replStatus := ptr.Deref(mdb.Status.Replication, mariadbv1alpha1.ReplicationStatus{})
	status, ok := replStatus.Replicas[pod]
	if !ok {
		return false
	}
	return replicaAuthResyncQualifies(mdb, status)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./pkg/controller/replication/ -run 'AuthResync|PodNeedsAuthResync' -v 2>&1 | tail -20`
Expected: PASS — `TestReplicaAuthResyncQualifies` (6 subtests) and `TestPodNeedsAuthResync` pass.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/mariadb_replication_types.go pkg/controller/replication/auth_resync.go pkg/controller/replication/auth_resync_test.go
git commit -m "feat(replication): add reconnectOnAuthError field + auth-resync predicate"
```

---

### Task 2: Reconcile-path hook

**Files:**
- Modify: `pkg/controller/replication/controller.go` (the role-gate block ~line 263-268 in `ReconcileReplicationInPod`)

**Interfaces:**
- Consumes: `r.podNeedsAuthResync` (Task 1), `topology.ConfigureReplica`, `WithResetMaster` (both `pkg/controller/replication/topology.go`), `req.replClientSet.clientForIndex`.
- Produces: no new exported symbols (behavior change only).

- [ ] **Step 1: Apply the hook**

In `pkg/controller/replication/controller.go`, replace the existing role-gate block:

```go
	if !opts.forceReplicaConfiguration {
		role, ok := replRoles[pod]
		if ok && role == mariadbv1alpha1.ReplicationRoleReplica {
			return ctrl.Result{}, nil
		}
	}
```

with:

```go
	if !opts.forceReplicaConfiguration {
		role, ok := replRoles[pod]
		if ok && role == mariadbv1alpha1.ReplicationRoleReplica {
			// Self-heal a replica stuck on repl auth error (1045): the normal role-gated
			// reconcile never re-applies its master credentials, so a repl-password
			// divergence leaves it broken forever. When opted in, re-issue CHANGE MASTER
			// with the current secret (lightweight: no RESET MASTER).
			if !r.podNeedsAuthResync(req.mariadb, pod) {
				return ctrl.Result{}, nil
			}
			logger.Info("Replica IO auth error (1045) detected, re-syncing master credentials", "pod", pod)
			client, err := req.replClientSet.clientForIndex(ctx, podIndex)
			if err != nil {
				logger.V(1).Info("error getting replica client", "err", err, "pod", pod)
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			if err := topology.ConfigureReplica(ctx, client, primaryPodIndex, WithResetMaster(false)); err != nil {
				return ctrl.Result{}, fmt.Errorf("error re-syncing replica credentials: %v", err)
			}
			return ctrl.Result{}, nil
		}
	}
```

Note: the existing fall-through path below (for pods NOT already in the `Replica` role, and for `forceReplicaConfiguration`) is unchanged.

- [ ] **Step 2: Build and vet**

Run: `go build ./pkg/controller/replication/... && go vet ./pkg/controller/replication/...`
Expected: exit 0, no output.

- [ ] **Step 3: Run the replication package tests (regression)**

Run: `go test ./pkg/controller/replication/ 2>&1 | tail -5`
Expected: `ok  github.com/mariadb-operator/mariadb-operator/v26/pkg/controller/replication` — all existing tests (switchover, configure-primary, changemaster) plus the new ones pass.

- [ ] **Step 4: Commit**

```bash
git add pkg/controller/replication/controller.go
git commit -m "feat(replication): re-sync replica master credentials on sustained auth error"
```

---

### Task 3: Regenerate CRDs, deepcopy, and API reference docs

**Files:**
- Regenerate (do not hand-edit): `api/v1alpha1/zz_generated.deepcopy.go`, `config/crd/bases/k8s.mariadb.com_mariadbs.yaml`, `deploy/charts/mariadb-operator-crds/templates/crds.yaml`, `deploy/crds/crds.yaml`, `docs/api_reference.md`

**Interfaces:** none (generated artifacts only).

- [ ] **Step 1: Run the dockerless generators**

Run (installs codegen tools to `bin/` on first run; `helm-crds`/`docs-api` avoid the Docker-based helm-docs):
```bash
make manifests code helm-crds docs-api
```
Expected: completes without error; regenerates CRDs, deepcopy, and `docs/api_reference.md`.

- [ ] **Step 2: Verify only the expected files changed and the new field is present**

Run:
```bash
git status --short | grep -vE '^\?\?'
grep -rn "reconnectOnAuthError" config/crd/bases/k8s.mariadb.com_mariadbs.yaml docs/api_reference.md | head
```
Expected: changed files are only the generated set above; `reconnectOnAuthError` appears in the CRD schema and `api_reference.md`. If any unrelated file changed (e.g. chart README), discard it — this task only regenerates schema/docs.

- [ ] **Step 3: Verify idempotency (re-run produces no further diff)**

Run: `make manifests code helm-crds docs-api && git status --porcelain | grep -vE '^\?\?' | wc -l`
Expected: the count is unchanged from Step 2 (generators are idempotent; matches what CI's Artifacts "Check diff" enforces).

- [ ] **Step 4: Full build + vet + test gate**

Run (api correctness is covered by `go build` + the Task 1 predicate tests, which exercise the helper; the api package's own suite is Ginkgo/envtest and is left to CI):
```bash
go build ./... && go vet ./... && go test ./pkg/controller/replication/ 2>&1 | tail -10
```
Expected: build/vet exit 0; replication tests pass.

- [ ] **Step 5: Commit**

```bash
git add api/v1alpha1/zz_generated.deepcopy.go config/crd deploy/charts/mariadb-operator-crds/templates/crds.yaml deploy/crds/crds.yaml docs/api_reference.md
git commit -m "chore: regenerate CRDs and API reference for reconnectOnAuthError"
```

---

## Post-merge rollout (ops, not code — do after the PR merges)

1. Open a PR from `feat/replica-reconnect-on-auth-error`; confirm CI green (build, unit, integration, helm, Artifacts).
2. Merge to `main`; CI publishes `ghcr.io/ubc/mariadb-operator:<sha>` and the fork charts.
3. `helm upgrade` the appcloud operator to the new image + chart (see memory `mariadb-operator-appcloud-deploy`); expect the fleet-wide agent roll.
4. Set `spec.replication.replica.recovery: { enabled: false, reconnectOnAuthError: true }` on the 7 hotcrp DBs (in `k8s-config`) so future desyncs self-heal. NOTE: `enabled` is a required CRD field — the manifest MUST include it (`false` keeps the backup-rebuild recovery off); omitting it is rejected at apply time.
5. (Optional) Verify by forcing a controlled desync on a low-risk DB (e.g. `hotcrp-test-db`) and confirming the operator re-syncs within the threshold window.
