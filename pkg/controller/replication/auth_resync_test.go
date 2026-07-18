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
