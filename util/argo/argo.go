package argo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/argoproj/argo-cd/engine/util/misc"

	log "github.com/sirupsen/logrus"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/argoproj/argo-cd/common"
	"github.com/argoproj/argo-cd/engine/util/git"
	"github.com/argoproj/argo-cd/engine/util/helm"
	"github.com/argoproj/argo-cd/engine/util/kube"
	argoappv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/pkg/client/clientset/versioned/typed/application/v1alpha1"
	"github.com/argoproj/argo-cd/reposerver/apiclient"
	"github.com/argoproj/argo-cd/util/db"
)

const (
	errDestinationMissing = "Destination server and/or namespace missing from app spec"
)

// FilterByProjects returns applications which belongs to the specified project
func FilterByProjects(apps []argoappv1.Application, projects []string) []argoappv1.Application {
	if len(projects) == 0 {
		return apps
	}
	projectsMap := make(map[string]bool)
	for i := range projects {
		projectsMap[projects[i]] = true
	}
	items := make([]argoappv1.Application, 0)
	for i := 0; i < len(apps); i++ {
		a := apps[i]
		if _, ok := projectsMap[a.Spec.GetProject()]; ok {
			items = append(items, a)
		}
	}
	return items

}

// RefreshApp updates the refresh annotation of an application to coerce the controller to process it
func RefreshApp(appIf v1alpha1.ApplicationInterface, name string, refreshType argoappv1.RefreshType) (*argoappv1.Application, error) {
	metadata := map[string]interface{}{
		"metadata": map[string]interface{}{
			"annotations": map[string]string{
				common.AnnotationKeyRefresh: string(refreshType),
			},
		},
	}
	var err error
	patch, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 5; attempt++ {
		app, err := appIf.Patch(name, types.MergePatchType, patch)
		if err != nil {
			if !apierr.IsConflict(err) {
				return nil, err
			}
		} else {
			log.Infof("Requested app '%s' refresh", name)
			return app, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, err
}

// WaitForRefresh watches an application until its comparison timestamp is after the refresh timestamp
// If refresh timestamp is not present, will use current timestamp at time of call
func WaitForRefresh(ctx context.Context, appIf v1alpha1.ApplicationInterface, name string, timeout *time.Duration) (*argoappv1.Application, error) {
	var cancel context.CancelFunc
	if timeout != nil {
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	ch := kube.WatchWithRetry(ctx, func() (i watch.Interface, e error) {
		fieldSelector := fields.ParseSelectorOrDie(fmt.Sprintf("metadata.name=%s", name))
		listOpts := metav1.ListOptions{FieldSelector: fieldSelector.String()}
		return appIf.Watch(listOpts)
	})
	for next := range ch {
		if next.Error != nil {
			return nil, next.Error
		}
		app, ok := next.Object.(*argoappv1.Application)
		if !ok {
			return nil, fmt.Errorf("Application event object failed conversion: %v", next)
		}
		annotations := app.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		if _, ok := annotations[common.AnnotationKeyRefresh]; !ok {
			return app, nil
		}
	}
	return nil, fmt.Errorf("application refresh deadline exceeded")
}

func TestRepoWithKnownType(repo *argoappv1.Repository, isHelm bool) error {
	repo = repo.DeepCopy()
	if isHelm {
		repo.Type = "helm"
	} else {
		repo.Type = "git"
	}
	return TestRepo(repo)
}

func TestRepo(repo *argoappv1.Repository) error {
	checks := map[string]func() error{
		"git": func() error {
			return git.TestRepo(repo.Repo, repo.GetGitCreds(), repo.IsInsecure(), repo.IsLFSEnabled())
		},
		"helm": func() error {
			_, err := helm.NewClient(repo.Repo, repo.GetHelmCreds()).GetIndex()
			return err
		},
	}
	if check, ok := checks[repo.Type]; ok {
		return check()
	}
	var err error
	for _, check := range checks {
		err = check()
		if err == nil {
			return nil
		}
	}
	return err
}

// ValidateRepo validates the repository specified in application spec. Following is checked:
// * the repository is accessible
// * the path contains valid manifests
// * there are parameters of only one app source type
// * ksonnet: the specified environment exists
func ValidateRepo(
	ctx context.Context,
	app *argoappv1.Application,
	repoClientset apiclient.Clientset,
	db db.ArgoDB,
	kustomizeOptions *argoappv1.KustomizeOptions,
	plugins []*argoappv1.ConfigManagementPlugin,
	kubectl kube.Kubectl,
) ([]argoappv1.ApplicationCondition, error) {
	spec := &app.Spec
	conditions := make([]argoappv1.ApplicationCondition, 0)

	// Test the repo
	conn, repoClient, err := repoClientset.NewRepoServerClient()
	if err != nil {
		return nil, err
	}
	defer misc.Close(conn)
	repo, err := db.GetRepository(ctx, spec.Source.RepoURL)
	if err != nil {
		return nil, err
	}

	repoAccessible := false
	err = TestRepoWithKnownType(repo, app.Spec.Source.IsHelm())
	if err != nil {
		conditions = append(conditions, argoappv1.ApplicationCondition{
			Type:    argoappv1.ApplicationConditionInvalidSpecError,
			Message: fmt.Sprintf("repository not accessible: %v", err),
		})
	} else {
		repoAccessible = true
	}

	// Verify only one source type is defined
	_, err = spec.Source.ExplicitType()
	if err != nil {
		return nil, err
	}

	// is the repo inaccessible - abort now
	if !repoAccessible {
		return conditions, nil
	}

	helmRepos, err := db.ListHelmRepositories(ctx)
	if err != nil {
		return nil, err
	}

	// get the app details, and populate the Ksonnet stuff from it
	appDetails, err := repoClient.GetAppDetails(ctx, &apiclient.RepoServerAppDetailsQuery{
		Repo:             repo,
		Source:           &spec.Source,
		Repos:            helmRepos,
		KustomizeOptions: kustomizeOptions,
	})
	if err != nil {
		conditions = append(conditions, argoappv1.ApplicationCondition{
			Type:    argoappv1.ApplicationConditionInvalidSpecError,
			Message: fmt.Sprintf("Unable to get app details: %v", err),
		})
		return conditions, nil
	}

	enrichSpec(spec, appDetails)

	cluster, err := db.GetCluster(context.Background(), spec.Destination.Server)
	if err != nil {
		conditions = append(conditions, argoappv1.ApplicationCondition{
			Type:    argoappv1.ApplicationConditionInvalidSpecError,
			Message: fmt.Sprintf("Unable to get cluster: %v", err),
		})
		return conditions, nil
	}
	cluster.ServerVersion, err = kubectl.GetServerVersion(cluster.RESTConfig())
	if err != nil {
		return nil, err
	}
	conditions = append(conditions, verifyGenerateManifests(ctx, repo, helmRepos, app, repoClient, kustomizeOptions, plugins, cluster.ServerVersion)...)

	return conditions, nil
}

func enrichSpec(spec *argoappv1.ApplicationSpec, appDetails *apiclient.RepoAppDetailsResponse) {
	if spec.Source.Ksonnet != nil && appDetails.Ksonnet != nil {
		env, ok := appDetails.Ksonnet.Environments[spec.Source.Ksonnet.Environment]
		if ok {
			// If server and namespace are not supplied, pull it from the app.yaml
			if spec.Destination.Server == "" {
				spec.Destination.Server = env.Destination.Server
			}
			if spec.Destination.Namespace == "" {
				spec.Destination.Namespace = env.Destination.Namespace
			}
		}
	}
}

// verifyGenerateManifests verifies a repo path can generate manifests
func verifyGenerateManifests(
	ctx context.Context,
	repoRes *argoappv1.Repository,
	helmRepos argoappv1.Repositories,
	app *argoappv1.Application,
	repoClient apiclient.RepoServerServiceClient,
	kustomizeOptions *argoappv1.KustomizeOptions,
	plugins []*argoappv1.ConfigManagementPlugin,
	kubeVersion string,
) []argoappv1.ApplicationCondition {
	spec := &app.Spec
	var conditions []argoappv1.ApplicationCondition
	if spec.Destination.Server == "" || spec.Destination.Namespace == "" {
		conditions = append(conditions, argoappv1.ApplicationCondition{
			Type:    argoappv1.ApplicationConditionInvalidSpecError,
			Message: errDestinationMissing,
		})
	}

	req := apiclient.ManifestRequest{
		Repo: &argoappv1.Repository{
			Repo: spec.Source.RepoURL,
			Type: repoRes.Type,
			Name: repoRes.Name,
		},
		Repos:             helmRepos,
		Revision:          spec.Source.TargetRevision,
		AppLabelValue:     app.Name,
		Namespace:         spec.Destination.Namespace,
		ApplicationSource: &spec.Source,
		Plugins:           plugins,
		KustomizeOptions:  kustomizeOptions,
		KubeVersion:       kubeVersion,
	}
	req.Repo.CopyCredentialsFrom(repoRes)

	// Only check whether we can access the application's path,
	// and not whether it actually contains any manifests.
	_, err := repoClient.GenerateManifest(ctx, &req)
	if err != nil {
		conditions = append(conditions, argoappv1.ApplicationCondition{
			Type:    argoappv1.ApplicationConditionInvalidSpecError,
			Message: fmt.Sprintf("Unable to generate manifests in %s: %v", spec.Source.Path, err),
		})
	}

	return conditions
}
