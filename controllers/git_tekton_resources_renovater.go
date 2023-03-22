/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	appstudiov1alpha1 "github.com/redhat-appstudio/application-api/api/v1alpha1"
	"github.com/redhat-appstudio/application-service/gitops"
	gitopsprepare "github.com/redhat-appstudio/application-service/gitops/prepare"
	buildappstudiov1alpha1 "github.com/redhat-appstudio/build-service/api/v1alpha1"
	"github.com/redhat-appstudio/build-service/pkg/github"
	batch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	RenovateConfigName          = "renovate-config"
	RenovateImageEnvName        = "RENOVATE_IMAGE"
	DefaultRenovateImageUrl     = "quay.io/redhat-appstudio/renovate:34.154-slim"
	DefaultRenovateMatchPattern = "^quay.io/redhat-appstudio-tekton-catalog/"
	RenovateMatchPatternEnvName = "RENOVATE_PATTERN"
	TimeToLiveOfJob             = 24 * time.Hour
	NextReconcile               = 10 * time.Hour
	InstallationsPerJob         = 20
	InstallationsPerJobEnvName  = "RENOVATE_INSTALLATIONS_PER_JOB"
)

// GitTektonResourcesRenovater watches AppStudio BuildPipelineSelector object in order to update
// existing .tekton directories.
type GitTektonResourcesRenovater struct {
	Client        client.Client
	Scheme        *runtime.Scheme
	Log           logr.Logger
	EventRecorder record.EventRecorder
}

type installationStruct struct {
	id           int
	token        string
	repositories []renovateRepository
}

type renovateRepository struct {
	Repository   string   `json:"repository"`
	BaseBranches []string `json:"baseBranches,omitempty"`
}

// SetupWithManager sets up the controller with the Manager.
func (r *GitTektonResourcesRenovater) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&buildappstudiov1alpha1.BuildPipelineSelector{}, builder.WithPredicates(predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return e.Object.GetNamespace() == buildServiceNamespaceName && e.Object.GetName() == buildPipelineSelectorResourceName
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectNew.GetNamespace() == buildServiceNamespaceName && e.ObjectNew.GetName() == buildPipelineSelectorResourceName
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	})).Complete(r)
}

// Set Role for managing jobs/configmaps/secrets in the controller namespace

// +kubebuilder:rbac:namespace=system,groups=batch,resources=jobs,verbs=create;get;list;watch;delete;deletecollection
// +kubebuilder:rbac:namespace=system,groups=core,resources=secrets,verbs=get;list;watch;create;patch;update;delete;deletecollection
// +kubebuilder:rbac:namespace=system,groups=core,resources=configmaps,verbs=get;list;watch;create;patch;update;delete;deletecollection

// +kubebuilder:rbac:groups=appstudio.redhat.com,resources=components,verbs=get;list

func (r *GitTektonResourcesRenovater) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {

	// Check if GitHub Application is used, if not then skip
	pacSecret := corev1.Secret{}
	globalPaCSecretKey := types.NamespacedName{Namespace: buildServiceNamespaceName, Name: gitopsprepare.PipelinesAsCodeSecretName}
	if err := r.Client.Get(ctx, globalPaCSecretKey, &pacSecret); err != nil {
		if !errors.IsNotFound(err) {
			r.EventRecorder.Event(&pacSecret, "Warning", "ErrorReadingPaCSecret", err.Error())
			r.Log.Error(err, "failed to get Pipelines as Code secret in %s namespace: %w", globalPaCSecretKey.Namespace, err)
			return ctrl.Result{}, nil
		}
	}
	isApp := gitops.IsPaCApplicationConfigured("github", pacSecret.Data)
	if !isApp {
		r.Log.Info("GitHub App is not set")
		return ctrl.Result{}, nil
	}

	// Load GitHub App and get GitHub Installations
	githubAppIdStr := string(pacSecret.Data[gitops.PipelinesAsCode_githubAppIdKey])
	githubAppId, err := strconv.ParseInt(githubAppIdStr, 10, 64)
	if err != nil {
		r.Log.Error(err, "failed to convert %s to int: %w", githubAppIdStr, err)
		return ctrl.Result{}, nil
	}
	privateKey := pacSecret.Data[gitops.PipelinesAsCode_githubPrivateKey]
	githubAppInstallations, slug, err := github.GetInstallations(githubAppId, privateKey)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Get Components
	componentList := &appstudiov1alpha1.ComponentList{}
	if err := r.Client.List(ctx, componentList, &client.ListOptions{}); err != nil {
		r.Log.Error(err, "failed to list Components")
		return ctrl.Result{}, err
	}
	componentUrlToBranchMap := make(map[string]string)
	for _, component := range componentList.Items {
		if component.Spec.Source.GitSource != nil {
			url := strings.TrimSuffix(strings.TrimSuffix(component.Spec.Source.GitSource.URL, ".git"), "/")
			componentUrlToBranchMap[url] = component.Spec.Source.GitSource.Revision
		}
	}

	// Match installed repositories with Components and get custom branch if defined
	installationsToUpdate := []installationStruct{}
	for _, githubAppInstallation := range githubAppInstallations {
		repositories := []renovateRepository{}
		for _, repository := range githubAppInstallation.Repositories {
			branch, ok := componentUrlToBranchMap[repository.GetHTMLURL()]
			// Filter repositories with installed GH App but missing Component
			if !ok {
				continue
			}
			baseBranches := []string{}
			if branch != "" {
				baseBranches = append(baseBranches, branch)
			}
			repositories = append(repositories, renovateRepository{
				BaseBranches: baseBranches,
				Repository:   repository.GetFullName(),
			})
		}
		// Do not add intatallation which has no matching repositories
		if len(repositories) == 0 {
			continue
		}
		installationsToUpdate = append(installationsToUpdate,
			installationStruct{
				id:           int(githubAppInstallation.ID),
				token:        githubAppInstallation.Token,
				repositories: repositories,
			})
	}

	// Generate renovate jobs. Limit processed installations per job.
	var installationPerJobInt int
	installationPerJobStr := os.Getenv(InstallationsPerJobEnvName)
	if regexp.MustCompile(`^\d{1,2}$`).MatchString(installationPerJobStr) {
		installationPerJobInt, _ = strconv.Atoi(installationPerJobStr)
		if installationPerJobInt == 0 {
			installationPerJobInt = InstallationsPerJob
		}
	} else {
		installationPerJobInt = InstallationsPerJob
	}
	for i := 0; i < len(installationsToUpdate); i += installationPerJobInt {
		end := i + installationPerJobInt

		if end > len(installationsToUpdate) {
			end = len(installationsToUpdate)
		}
		err = r.CreateRenovaterJob(ctx, installationsToUpdate[i:end], slug)
		if err != nil {
			r.Log.Error(err, "failed to create a job")
		}
	}

	return ctrl.Result{RequeueAfter: NextReconcile}, nil
}

func generateConfigJS(slug string, repositories []renovateRepository) string {
	repositoriesData, _ := json.Marshal(repositories)
	template := `
	module.exports = {
		platform: "github",
		username: "%s[bot]",
		gitAuthor:"%s <123456+%s[bot]@users.noreply.github.com>",
		onboarding: false,
		requireConfig: "ignored",
		enabledManagers: ["tekton"],
		repositories: %s,
		tekton: {
			fileMatch: ["\\.yaml$", "\\.yml$"],
			includePaths: [".tekton/**"],
			packageRules: [
			  {
				matchPackagePatterns: ["*"],
				enabled: false
			  },
			  {
				matchPackagePatterns: ["%s"],
				matchDepPatterns: ["%s"],
				groupName: "tekton references",
				enabled: true
			  }
			]
		},
		includeForks: true,
		dependencyDashboard: false
	}
	`
	renovatePattern := os.Getenv(RenovateMatchPatternEnvName)
	if renovatePattern == "" {
		renovatePattern = DefaultRenovateMatchPattern
	}
	return fmt.Sprintf(template, slug, slug, slug, repositoriesData, renovatePattern, renovatePattern)
}

func (r *GitTektonResourcesRenovater) CreateRenovaterJob(ctx context.Context, installations []installationStruct, slug string) error {
	if len(installations) == 0 {
		return nil
	}
	timestamp := time.Now().Unix()
	name := fmt.Sprintf("renovate-job-%d-%s", timestamp, getRandomString(5))
	secretTokens := map[string]string{}
	configmaps := map[string]string{}
	renovateCmds := []string{}
	for _, installation := range installations {
		secretTokens[fmt.Sprint(installation.id)] = installation.token
		configmaps[fmt.Sprintf("%d.js", installation.id)] = generateConfigJS(slug, installation.repositories)
		renovateCmds = append(renovateCmds,
			fmt.Sprintf("RENOVATE_TOKEN=$TOKEN_%d RENOVATE_CONFIG_FILE=/configs/%d.js renovate", installation.id, installation.id),
		)
	}
	if len(renovateCmds) == 0 {
		return nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: buildServiceNamespaceName,
		},
		StringData: secretTokens,
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: buildServiceNamespaceName,
		},
		Data: configmaps,
	}
	trueBool := true
	falseBool := false
	backoffLimit := int32(1)
	timeToLive := int32(TimeToLiveOfJob.Seconds())
	renovateImageUrl := os.Getenv(RenovateImageEnvName)
	if renovateImageUrl == "" {
		renovateImageUrl = DefaultRenovateImageUrl
	}
	job := &batch.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: buildServiceNamespaceName,
		},
		Spec: batch.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &timeToLive,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: name,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: name},
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:  "renovate",
							Image: renovateImageUrl,
							EnvFrom: []corev1.EnvFromSource{
								{
									Prefix: "TOKEN_",
									SecretRef: &corev1.SecretEnvSource{
										LocalObjectReference: corev1.LocalObjectReference{
											Name: name,
										},
									},
								},
							},
							Command: []string{"bash", "-c", strings.Join(renovateCmds, "; ")},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      name,
									MountPath: "/configs",
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
								RunAsNonRoot:             &trueBool,
								AllowPrivilegeEscalation: &falseBool,
								SeccompProfile: &corev1.SeccompProfile{
									Type: corev1.SeccompProfileTypeRuntimeDefault,
								},
							},
						},
					},
					RestartPolicy: corev1.RestartPolicyNever,
				},
			},
		},
	}

	if err := r.Client.Create(ctx, secret); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, configMap); err != nil {
		return err
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return err
	}
	r.Log.Info(fmt.Sprintf("Job %s triggered", job.Name))
	if err := controllerutil.SetOwnerReference(job, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Update(ctx, secret); err != nil {
		return err
	}

	if err := controllerutil.SetOwnerReference(job, configMap, r.Scheme); err != nil {
		return err
	}
	if err := r.Client.Update(ctx, configMap); err != nil {
		return err
	}

	return nil
}