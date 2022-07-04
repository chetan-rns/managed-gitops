package argoprojio

import (
	"context"
	"fmt"
	"time"

	apierr "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/config/db"
	cache "github.com/redhat-appstudio/managed-gitops/backend-shared/config/db/util"
	"github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	sharedutil "github.com/redhat-appstudio/managed-gitops/backend-shared/util"
	appEventLoop "github.com/redhat-appstudio/managed-gitops/backend/eventloop/application_event_loop"
	"github.com/redhat-appstudio/managed-gitops/backend/util/fauxargocd"
	"github.com/redhat-appstudio/managed-gitops/cluster-agent/controllers"
	"gopkg.in/yaml.v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	appRowBatchSize             = 50               // Number of rows needs to be fetched in each batch.
	namespaceReconcilerInterval = 30 * time.Minute // Interval in Minutes to reconcile workspace/namespace.
	sleepIntervalsOfBatches     = 1 * time.Second  // Interval in Millisecond between each batch.
)

// This function iterates through each Workspace/Namespace present in DB and ensures that the state of resources in Cluster is in Sync with DB.
func (r *ApplicationReconciler) StartNamespaceReconciler() {
	r.startTimerForNextCycle()
}

func (r *ApplicationReconciler) startTimerForNextCycle() {
	go func() {
		// Timer to trigger Reconciler
		timer := time.NewTimer(time.Duration(namespaceReconcilerInterval))
		<-timer.C

		ctx := context.Background()
		log := log.FromContext(ctx).WithValues("component", "namespace-reconciler")

		_, _ = util.CatchPanic(func() error {
			runNamespaceReconcile(ctx, r.DB, r.Client, log)
			return nil
		})

		// Kick off the timer again, once the old task runs.
		// This ensures that at least 'namespaceReconcilerInterval' time elapses from the end of one rum to the beginning of another.
		r.startTimerForNextCycle()
	}()

}

func runNamespaceReconcile(ctx context.Context, dbQueries db.DatabaseQueries, client client.Client, log logr.Logger) {

	// Fetch list of ArgoCD applications to be used later
	processedApplicationIds := make(map[string]string)
	argoApplicationList := appv1.ApplicationList{}
	if err := client.List(ctx, &argoApplicationList); err != nil {
		log.Error(err, "Error occurred in Namespace Reconciler while fetching list of ArgoCD applications.")
	}
	argoApplications := argoApplicationList.Items

	offSet := 0

	// Delete operation resources created during previous run.
	cleanK8sOperations(ctx, dbQueries, client, log)

	// Get Special user from DB because we need ClusterUser for creating Operation and we don't have one.
	// Hence created a dummy Cluster User for internal purpose.
	var specialClusterUser db.ClusterUser
	if err := dbQueries.GetOrCreateSpecialClusterUser(ctx, &specialClusterUser); err != nil {
		log.Error(err, "Error occurred in Namespace Reconciler while fetching clusterUser.")
		return
	}

	log.Info("Triggered Namespace Reconciler to keep Argo application in sync with DB.")

	// Continuously iterate and fetch batches until all entries of Application table are processed.
	for {

		if offSet != 0 {
			time.Sleep(sleepIntervalsOfBatches)
		}

		var listOfApplicationsFromDB []db.Application
		var applicationFromDB fauxargocd.FauxApplication

		// Fetch Application table entries in batch size as configured above.​
		if err := dbQueries.GetApplicationBatch(ctx, &listOfApplicationsFromDB, appRowBatchSize, offSet); err != nil {
			log.Error(err, fmt.Sprintf("Error occurred in Namespace Reconciler while fetching batch from Offset: %d to %d: ",
				offSet, offSet+appRowBatchSize))
			break
		}

		// Break the loop if no entries are left in table to be processed.
		if len(listOfApplicationsFromDB) == 0 {
			log.Info("All Application entries are processed by Namespace Reconciler.")
			break
		}

		// Iterate over batch received above.
		for _, applicationRowFromDB := range listOfApplicationsFromDB {
			processedApplicationIds[applicationRowFromDB.Application_id] = applicationRowFromDB.Application_id

			// Fetch the Application object from DB
			if err := yaml.Unmarshal([]byte(applicationRowFromDB.Spec_field), &applicationFromDB); err != nil {
				log.Error(err, "Error occurred in Namespace Reconciler while unmarshalling application: "+applicationRowFromDB.Application_id)
				continue // Skip to next iteration instead of stopping the entire loop.
			}

			// Fetch the Application object from k8s
			applicationFromArgoCD := appv1.Application{}
			namespacedName := types.NamespacedName{
				Name:      applicationFromDB.Name,
				Namespace: applicationFromDB.Namespace}

			err := client.Get(ctx, namespacedName, &applicationFromArgoCD)
			if err != nil {
				if apierr.IsNotFound(err) {
					log.Info("Application " + applicationRowFromDB.Application_id + " not found in ArgoCD, probably user deleted it, " +
						"but It still exists in DB, hence recreating application in ArgoCD.")

					// We need to recreate ArgoCD Application, to do that create Operation to inform ArgoCD about it.
					dbOperationInput := db.Operation{
						Instance_id:   applicationRowFromDB.Engine_instance_inst_id,
						Resource_id:   applicationRowFromDB.Application_id,
						Resource_type: db.OperationResourceType_Application,
					}

					_, _, err = appEventLoop.CreateOperation(ctx, false, dbOperationInput,
						specialClusterUser.Clusteruser_id, cache.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, client, log)
					if err != nil {
						log.Error(err, "Namespace Reconciler is unable to create operation: "+dbOperationInput.ShortString())
					}
					log.Info("Operation is created to recreateArgoCD  Application " + applicationRowFromDB.Application_id)
					continue
				} else {
					log.Error(err, "Error occurred in Namespace Reconciler while fetching application from cluster: "+applicationRowFromDB.Application_id)
					continue
				}
			}

			// At this point we have the applications from ArgoCD and DB, now compare them to check if they are not in Sync.
			if compareApplications(applicationFromArgoCD, applicationFromDB, log) {
				log.V(sharedutil.LogLevel_Debug).Info("Argo application is in Sync with DB, Application:" + applicationRowFromDB.Application_id)
				continue
			} else {
				log.Info("Argo application is not in Sync with DB, updating Argo CD App. Application:" + applicationRowFromDB.Application_id)
			}

			// At this point application from ArgoCD and DB are not in Sync, so need to update ArgoCD application according to DB entry

			// ArgoCD application and DB entry are not in Sync,
			// ArgoCD should use the state of resources present in the database should
			// Create Operation to inform ArgoCD to get in Sync with database entry.
			dbOperationInput := db.Operation{
				Instance_id:   applicationRowFromDB.Engine_instance_inst_id,
				Resource_id:   applicationRowFromDB.Application_id,
				Resource_type: db.OperationResourceType_Application,
			}

			_, _, err = appEventLoop.CreateOperation(ctx, false, dbOperationInput,
				specialClusterUser.Clusteruser_id, cache.GetGitOpsEngineSingleInstanceNamespace(), dbQueries, client, log)
			if err != nil {
				log.Error(err, "Namespace Reconciler is unable to create operation: "+dbOperationInput.ShortString())
				continue
			}

			log.Info("Namespace Reconcile processed application: " + applicationRowFromDB.Application_id)
		}

		// Skip processed entries in next iteration
		offSet += appRowBatchSize
	}

	// Start a goroutine, because DeleteArgoCDApplication() function from cluster-agent/controllers may take some time to delete application.
	go deleteOrphanedApplications(argoApplications, processedApplicationIds, ctx, client, log)

	log.Info(fmt.Sprintf("Namespace Reconciler finished an iteration at %s. "+
		"Next iteration will be triggered after %v Minutes", time.Now().String(), namespaceReconcilerInterval))
}

// compareApplications compares Application objects, since both objects are of different types we can not use == operator for comparison.
func compareApplications(applicationFromArgoCD appv1.Application, applicationFromDB fauxargocd.FauxApplication, log logr.Logger) bool {

	if applicationFromArgoCD.APIVersion != applicationFromDB.APIVersion {
		log.Info("APIVersion field in ArgoCD and DB entry is not in Sync.")
		log.Info("APIVersion:= ArgoCD: " + applicationFromArgoCD.APIVersion + "; DB: " + applicationFromDB.APIVersion)
		return false
	}
	if applicationFromArgoCD.Kind != applicationFromDB.Kind {
		log.Info("Kind field in ArgoCD and DB entry is not in Sync.")
		log.Info("Kind:= ArgoCD: " + applicationFromArgoCD.Kind + "; DB: " + applicationFromDB.Kind)
		return false
	}
	if applicationFromArgoCD.Name != applicationFromDB.Name {
		log.Info("Name field in ArgoCD and DB entry is not in Sync.")
		log.Info("Name:= ArgoCD: " + applicationFromArgoCD.Name + "; DB: " + applicationFromDB.Name)
		return false
	}
	if applicationFromArgoCD.Namespace != applicationFromDB.Namespace {
		log.Info("Namespace field in ArgoCD and DB entry is not in Sync.")
		log.Info("Namespace:= ArgoCD: " + applicationFromArgoCD.Namespace + "; DB: " + applicationFromDB.Namespace)
		return false
	}
	if applicationFromArgoCD.Spec.Source.RepoURL != applicationFromDB.Spec.Source.RepoURL {
		log.Info("RepoURL field in ArgoCD and DB entry is not in Sync.")
		log.Info("RepoURL:= ArgoCD: " + applicationFromArgoCD.Spec.Source.RepoURL + "; DB: " + applicationFromDB.Spec.Source.RepoURL)
		return false
	}
	if applicationFromArgoCD.Spec.Source.Path != applicationFromDB.Spec.Source.Path {
		log.Info("Path field in ArgoCD and DB entry is not in Sync.")
		log.Info("Path:= ArgoCD: " + applicationFromArgoCD.Spec.Source.Path + "; DB: " + applicationFromDB.Spec.Source.Path)
		return false
	}
	if applicationFromArgoCD.Spec.Source.TargetRevision != applicationFromDB.Spec.Source.TargetRevision {
		log.Info("TargetRevision field in ArgoCD and DB entry is not in Sync.")
		log.Info("TargetRevision:= ArgoCD: " + applicationFromArgoCD.Spec.Source.TargetRevision + "; DB: " + applicationFromDB.Spec.Source.TargetRevision)
		return false
	}
	if applicationFromArgoCD.Spec.Destination.Server != applicationFromDB.Spec.Destination.Server {
		log.Info("Destination.Server field in ArgoCD and DB entry is not in Sync.")
		log.Info("Destination.Server:= ArgoCD: " + applicationFromArgoCD.Spec.Destination.Server + "; DB: " + applicationFromDB.Spec.Destination.Server)
		return false
	}
	if applicationFromArgoCD.Spec.Destination.Namespace != applicationFromDB.Spec.Destination.Namespace {
		log.Info("Destination.Namespace field in ArgoCD and DB entry is not in Sync.")
		log.Info("Destination.Namespace:= ArgoCD: " + applicationFromArgoCD.Spec.Destination.Namespace + "; DB: " + applicationFromDB.Spec.Destination.Namespace)
		return false
	}
	if applicationFromArgoCD.Spec.Destination.Name != applicationFromDB.Spec.Destination.Name {
		log.Info("Destination.Name field in ArgoCD and DB entry is not in Sync.")
		log.Info("Destination.Name:= ArgoCD: " + applicationFromArgoCD.Spec.Destination.Name + "; DB: " + applicationFromDB.Spec.Destination.Name)
		return false
	}
	if applicationFromArgoCD.Spec.Project != applicationFromDB.Spec.Project {
		log.Info("Project field in ArgoCD and DB entry is not in Sync.")
		log.Info("Project:= ArgoCD: " + applicationFromArgoCD.Spec.Project + "; DB: " + applicationFromDB.Spec.Project)
		return false
	}
	if applicationFromArgoCD.Spec.SyncPolicy.Automated.Prune != applicationFromDB.Spec.SyncPolicy.Automated.Prune {
		log.Info("Prune field in ArgoCD and DB entry is not in Sync.")
		return false
	}
	if applicationFromArgoCD.Spec.SyncPolicy.Automated.SelfHeal != applicationFromDB.Spec.SyncPolicy.Automated.SelfHeal {
		log.Info("SelfHeal field in ArgoCD and DB entry is not in Sync.")
		return false
	}
	if applicationFromArgoCD.Spec.SyncPolicy.Automated.AllowEmpty != applicationFromDB.Spec.SyncPolicy.Automated.AllowEmpty {
		log.Info("AllowEmpty field in ArgoCD and DB entry is not in Sync.")
		return false
	}
	return true
}

func cleanK8sOperations(ctx context.Context, dbq db.DatabaseQueries, client client.Client, log logr.Logger) {
	// Get list of Operations from cluster.
	listOfK8sOperation := v1alpha1.OperationList{}
	err := client.List(ctx, &listOfK8sOperation)
	if err != nil {
		log.Error(err, "Unable to fetch list of k8s Operation from cluster.")
		return
	}

	for _, k8sOperation := range listOfK8sOperation.Items {

		// Skip if Operation was not created by Namespace Reconciler.
		if k8sOperation.Annotations[appEventLoop.IdentifierKey] != appEventLoop.IdentifierValue {
			continue
		}

		// Fetch corresponding DB entry
		dbOperation := db.Operation{
			Operation_id: k8sOperation.Spec.OperationID,
		}
		if err := dbq.GetOperationById(ctx, &dbOperation); err != nil {
			continue
		}

		if dbOperation.State != db.OperationState_Completed && dbOperation.State != db.OperationState_Failed {
			log.V(sharedutil.LogLevel_Debug).Info("K8s Operation is not ready for cleanup : " + string(k8sOperation.UID) + " DbOperation: " + string(k8sOperation.Spec.OperationID))
			continue
		}

		log.Info("Deleting Operation created by Namespace Reconciler." + string(k8sOperation.UID))

		// Delete the k8s operation now.
		if err := appEventLoop.CleanupOperation(ctx, dbOperation, k8sOperation, cache.GetGitOpsEngineSingleInstanceNamespace(), dbq, client, log); err != nil {
			log.Error(err, "Unable to Delete k8s Operation"+string(k8sOperation.UID)+" for DbOperation: "+string(k8sOperation.Spec.OperationID))
		} else {
			log.Info("Deleted k8s Operation: " + string(k8sOperation.UID) + " for DbOperation: " + string(k8sOperation.Spec.OperationID))
		}
	}
	log.V(sharedutil.LogLevel_Debug).Info("Cleaned all Operations created by Namespace Reconciler.")
}

func deleteOrphanedApplications(argoApplications []appv1.Application, processedApplicationIds map[string]string, ctx context.Context, client client.Client, log logr.Logger) []appv1.Application {
	log.V(sharedutil.LogLevel_Debug).Info("Looking for any orphaned ArgoCD application to delete.")

	// Iterate through all application IDs collected from DB.
	for id := range processedApplicationIds {
		// Iterate through ArgoCD all and remove corresponding application from list.
		for i, application := range argoApplications {
			if application.Labels["databaseID"] == id {
				argoApplications = dropProcessedApplicationId(argoApplications, i)
			}
		}
	}

	log.Info(fmt.Sprintf("Found %d orphaned ArgoCD application to be deleted.", len(argoApplications)))
	// Delete all applications which are not having entry in DB.
	deleteApplication(ctx, client, argoApplications, log)

	return argoApplications
}

func dropProcessedApplicationId(argoApplications []appv1.Application, i int) []appv1.Application {
	argoApplications[i] = argoApplications[len(argoApplications)-1]
	return argoApplications[:len(argoApplications)-1]
}

func deleteApplication(ctx context.Context, client client.Client, argoApplications []appv1.Application, log logr.Logger) {
	for _, application := range argoApplications {
		if err := controllers.DeleteArgoCDApplication(ctx, application, client, log); err != nil {
			log.Error(err, "unable to delete an orphaned ArgoCD application "+application.Name)
		} else {
			log.Info("Deleted orphaned ArgoCD application " + application.Name)
		}
	}
}
