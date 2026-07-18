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
