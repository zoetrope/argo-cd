package argo

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/engine/util/argo"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/watch"
	testcore "k8s.io/client-go/testing"

	argoappv1 "github.com/argoproj/argo-cd/engine/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/engine/pkg/client/clientset/versioned/fake"
	"github.com/argoproj/argo-cd/reposerver/apiclient"
)

func TestRefreshApp(t *testing.T) {
	var testApp argoappv1.Application
	testApp.Name = "test-app"
	testApp.Namespace = "default"
	appClientset := appclientset.NewSimpleClientset(&testApp)
	appIf := appClientset.ArgoprojV1alpha1().Applications("default")
	_, err := argo.RefreshApp(appIf, "test-app", argoappv1.RefreshTypeNormal)
	assert.Nil(t, err)
	// For some reason, the fake Application inferface doesn't reflect the patch status after Patch(),
	// so can't verify it was set in unit tests.
	//_, ok := newApp.Annotations[common.AnnotationKeyRefresh]
	//assert.True(t, ok)
}

func TestWaitForRefresh(t *testing.T) {
	appClientset := appclientset.NewSimpleClientset()

	// Verify timeout
	appIf := appClientset.ArgoprojV1alpha1().Applications("default")
	oneHundredMs := 100 * time.Millisecond
	app, err := WaitForRefresh(context.Background(), appIf, "test-app", &oneHundredMs)
	assert.NotNil(t, err)
	assert.Nil(t, app)
	assert.Contains(t, strings.ToLower(err.Error()), "deadline exceeded")

	// Verify success
	var testApp argoappv1.Application
	testApp.Name = "test-app"
	testApp.Namespace = "default"
	appClientset = appclientset.NewSimpleClientset()

	appIf = appClientset.ArgoprojV1alpha1().Applications("default")
	watcher := watch.NewFake()
	appClientset.PrependWatchReactor("applications", testcore.DefaultWatchReactor(watcher, nil))
	// simulate add/update/delete watch events
	go watcher.Add(&testApp)
	app, err = WaitForRefresh(context.Background(), appIf, "test-app", &oneHundredMs)
	assert.Nil(t, err)
	assert.NotNil(t, app)
}

func Test_enrichSpec(t *testing.T) {
	t.Run("Empty", func(t *testing.T) {
		spec := &argoappv1.ApplicationSpec{}
		enrichSpec(spec, &apiclient.RepoAppDetailsResponse{})
		assert.Empty(t, spec.Destination.Server)
		assert.Empty(t, spec.Destination.Namespace)
	})
	t.Run("Ksonnet", func(t *testing.T) {
		spec := &argoappv1.ApplicationSpec{
			Source: argoappv1.ApplicationSource{
				Ksonnet: &argoappv1.ApplicationSourceKsonnet{
					Environment: "qa",
				},
			},
		}
		response := &apiclient.RepoAppDetailsResponse{
			Ksonnet: &apiclient.KsonnetAppSpec{
				Environments: map[string]*apiclient.KsonnetEnvironment{
					"prod": {
						Destination: &apiclient.KsonnetEnvironmentDestination{
							Server:    "my-server",
							Namespace: "my-namespace",
						},
					},
				},
			},
		}
		enrichSpec(spec, response)
		assert.Empty(t, spec.Destination.Server)
		assert.Empty(t, spec.Destination.Namespace)

		spec.Source.Ksonnet.Environment = "prod"
		enrichSpec(spec, response)
		assert.Equal(t, "my-server", spec.Destination.Server)
		assert.Equal(t, "my-namespace", spec.Destination.Namespace)
	})
}
