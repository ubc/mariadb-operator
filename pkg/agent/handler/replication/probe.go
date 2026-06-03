package replication

import (
	"context"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/v26/api/v1alpha1"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/agent/router"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/environment"
	mdbhttp "github.com/mariadb-operator/mariadb-operator/v26/pkg/http"
	"github.com/mariadb-operator/mariadb-operator/v26/pkg/sql"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type ReplicationProbe struct {
	mariadbKey      types.NamespacedName
	k8sClient       ctrlclient.Client
	env             *environment.PodEnvironment
	responseWriter  *mdbhttp.ResponseWriter
	livenessLogger  logr.Logger
	readinessLogger logr.Logger
}

var requestTimeout = 3 * time.Second

func NewReplicationProbe(env *environment.PodEnvironment, k8sClient ctrlclient.Client, responseWriter *mdbhttp.ResponseWriter,
	logger *logr.Logger) router.ProbeHandler {
	return &ReplicationProbe{
		mariadbKey: types.NamespacedName{
			Name:      env.MariadbName,
			Namespace: env.PodNamespace,
		},
		k8sClient:       k8sClient,
		env:             env,
		responseWriter:  responseWriter,
		livenessLogger:  logger.WithName("liveness"),
		readinessLogger: logger.WithName("readiness"),
	}
}

func (p *ReplicationProbe) Liveness(w http.ResponseWriter, r *http.Request) {
	p.livenessLogger.V(1).Info("Probe started")

	sqlCtx, sqlCancel := context.WithTimeout(context.Background(), requestTimeout)
	defer sqlCancel()

	sqlClient, err := sql.NewLocalClientWithPodEnv(sqlCtx, p.env, sql.WithTimeout(requestTimeout))
	if err != nil {
		p.livenessLogger.Error(err, "error getting SQL client")
		p.responseWriter.WriteErrorf(w, "error getting SQL client: %v", err)
		return
	}
	defer sqlClient.Close()

	isReplica, err := sqlClient.IsReplicationReplica(sqlCtx)
	if err != nil {
		p.livenessLogger.Error(err, "error checking replica")
		p.responseWriter.WriteErrorf(w, "error checking replica: %v", err)
		return
	}
	if isReplica {
		// NOTE(ubc-fork): Liveness must NOT depend on replication thread state.
		// A stopped SQL/IO thread — e.g. during operator-driven STOP SLAVE /
		// CHANGE MASTER reconfiguration on startup, a transient replication stop,
		// or replica lag — is a *readiness* concern (handled by Readiness below),
		// not a reason to kill mariadbd. Failing liveness here makes kubelet
		// restart mariadbd, which re-triggers replication reconfiguration and
		// stops the threads again -> CrashLoopBackOff. The IsReplicationReplica()
		// check above already proves mariadbd is alive and responsive, which is
		// all liveness should assert.
		p.livenessLogger.V(1).Info("Replica is alive")
		p.responseWriter.WriteOK(w, nil)
		return
	}

	isPrimary, err := sqlClient.IsReplicationPrimary(sqlCtx)
	if err != nil {
		p.livenessLogger.Error(err, "error checking primary")
		p.responseWriter.WriteErrorf(w, "error checking primary: %v", err)
		return
	}
	if !isPrimary {
		p.livenessLogger.Error(nil, "Primary not configured")
		p.responseWriter.WriteError(w, "Primary not configured")
		return
	}

	p.responseWriter.WriteOK(w, nil)
}

func (p *ReplicationProbe) Readiness(w http.ResponseWriter, r *http.Request) {
	p.readinessLogger.V(1).Info("Probe started")

	sqlCtx, sqlCancel := context.WithTimeout(context.Background(), requestTimeout)
	defer sqlCancel()

	k8sCtx, k8sCancel := context.WithTimeout(context.Background(), requestTimeout)
	defer k8sCancel()

	sqlClient, err := sql.NewLocalClientWithPodEnv(sqlCtx, p.env, sql.WithTimeout(requestTimeout))
	if err != nil {
		p.readinessLogger.Error(err, "error getting SQL client")
		p.responseWriter.WriteErrorf(w, "error getting SQL client: %v", err)
		return
	}
	defer sqlClient.Close()

	isReplica, err := sqlClient.IsReplicationReplica(sqlCtx)
	if err != nil {
		p.readinessLogger.Error(err, "error checking replica")
		p.responseWriter.WriteErrorf(w, "error checking replica: %v", err)
		return
	}
	if isReplica {
		status, err := sqlClient.ReplicaStatus(sqlCtx, p.readinessLogger)
		if err != nil {
			p.readinessLogger.Error(err, "error getting replica status")
			p.responseWriter.WriteErrorf(w, "error getting replica status: %v", err)
			return
		}
		if status.SecondsBehindMaster == nil {
			p.readinessLogger.Error(nil, "could not determine replica lag")
			p.responseWriter.WriteError(w, "could not determine replica lag")
			return
		}
		secondsBehindMaster := *status.SecondsBehindMaster

		maxLagSeconds := p.getMaxLagSeconds(k8sCtx)
		if secondsBehindMaster > maxLagSeconds {
			p.readinessLogger.Error(nil, "Replica is lagging behind master", "seconds", secondsBehindMaster, "max-seconds", maxLagSeconds)
			p.responseWriter.WriteErrorf(w, "Replica is lagging %d seconds behind master (max seconds: %d)", secondsBehindMaster, maxLagSeconds)
			return
		}

		p.readinessLogger.V(1).Info(
			"Replica lag status",
			"seconds", secondsBehindMaster,
			"max-seconds", maxLagSeconds,
		)
		p.responseWriter.WriteOK(w, nil)
		return
	}

	isPrimary, err := sqlClient.IsReplicationPrimary(sqlCtx)
	if err != nil {
		p.readinessLogger.Error(err, "error checking primary")
		p.responseWriter.WriteErrorf(w, "error checking primary: %v", err)
		return
	}
	if !isPrimary {
		p.readinessLogger.Error(nil, "Primary not configured")
		p.responseWriter.WriteError(w, "Primary not configured")
		return
	}

	p.responseWriter.WriteOK(w, nil)
}

func (p *ReplicationProbe) getMaxLagSeconds(ctx context.Context) int {
	var mdb mariadbv1alpha1.MariaDB
	if err := p.k8sClient.Get(ctx, p.mariadbKey, &mdb); err != nil {
		p.readinessLogger.Error(err, "error getting MariaDB. Using default max replication lag")
		return 0
	}
	replication := ptr.Deref(mdb.Spec.Replication, mariadbv1alpha1.Replication{})
	replica := replication.Replica
	return ptr.Deref(replica.MaxLagSeconds, 0)
}
