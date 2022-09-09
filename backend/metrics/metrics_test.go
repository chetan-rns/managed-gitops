package metrics

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus/testutil"
	managedgitopsv1alpha1 "github.com/redhat-appstudio/managed-gitops/backend-shared/apis/managed-gitops/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
)

var _ = Describe("Test for Gitopsdeployment metrics counter", func() {

	FContext("Prometheus metrics responds to count of active/failed GitopsDeployments", func() {
		It("tests Add/Update, Remove, SetError function on a gitops deployment", func() {

			ClearMetrics()

			numberOfGitOpsDeploymentsInErrorState := testutil.ToFloat64(GitopsdeplFailures)
			totalNumberOfGitOpsDeploymentMetrics := testutil.ToFloat64(Gitopsdepl)

			gitopsDepl := &managedgitopsv1alpha1.GitOpsDeployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-gitops-depl",
					Namespace: "gitops-depl-namespace",
					UID:       uuid.NewUUID(),
				},
				Spec: managedgitopsv1alpha1.GitOpsDeploymentSpec{
					Source: managedgitopsv1alpha1.ApplicationSource{
						RepoURL:        "https://github.com/abc-org/abc-repo",
						Path:           "/abc-path",
						TargetRevision: "abc-commit"},
					Type: managedgitopsv1alpha1.GitOpsDeploymentSpecType_Automated,
					Destination: managedgitopsv1alpha1.ApplicationDestination{
						Namespace: "abc-namespace",
					},
				},
			}

			AddOrUpdateGitOpsDeployment(gitopsDepl.Name, gitopsDepl.Namespace, string(gitopsDepl.UID))
			newTotalNumberOfGitOpsDeploymentMetrics := testutil.ToFloat64(Gitopsdepl)
			newNumberOfGitOpsDeploymentsInErrorState := testutil.ToFloat64(GitopsdeplFailures)
			Expect(newTotalNumberOfGitOpsDeploymentMetrics).To(Equal(totalNumberOfGitOpsDeploymentMetrics + 1))
			Expect(newNumberOfGitOpsDeploymentsInErrorState).To(Equal(numberOfGitOpsDeploymentsInErrorState))

			SetErrorState(gitopsDepl.Name, gitopsDepl.Namespace, string(gitopsDepl.UID), true)
			newTotalNumberOfGitOpsDeploymentMetrics = testutil.ToFloat64(Gitopsdepl)
			newNumberOfGitOpsDeploymentsInErrorState = testutil.ToFloat64(GitopsdeplFailures)
			Expect(newTotalNumberOfGitOpsDeploymentMetrics).To(Equal(totalNumberOfGitOpsDeploymentMetrics + 1))
			Expect(newNumberOfGitOpsDeploymentsInErrorState).To(Equal(numberOfGitOpsDeploymentsInErrorState + 1))

			SetErrorState(gitopsDepl.Name, gitopsDepl.Namespace, string(gitopsDepl.UID), false)
			newTotalNumberOfGitOpsDeploymentMetrics = testutil.ToFloat64(Gitopsdepl)
			newNumberOfGitOpsDeploymentsInErrorState = testutil.ToFloat64(GitopsdeplFailures)
			Expect(newTotalNumberOfGitOpsDeploymentMetrics).To(Equal(totalNumberOfGitOpsDeploymentMetrics + 1))
			Expect(newNumberOfGitOpsDeploymentsInErrorState).To(Equal(numberOfGitOpsDeploymentsInErrorState))

			RemoveGitOpsDeployment(gitopsDepl.Name, gitopsDepl.Namespace, string(gitopsDepl.UID))
			newTotalNumberOfGitOpsDeploymentMetrics = testutil.ToFloat64(Gitopsdepl)
			newNumberOfGitOpsDeploymentsInErrorState = testutil.ToFloat64(GitopsdeplFailures)
			Expect(newTotalNumberOfGitOpsDeploymentMetrics).To(Equal(totalNumberOfGitOpsDeploymentMetrics))
			Expect(newNumberOfGitOpsDeploymentsInErrorState).To(Equal(numberOfGitOpsDeploymentsInErrorState))

		})
	})
})
