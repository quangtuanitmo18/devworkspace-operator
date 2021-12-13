// Copyright (c) 2019-2021 Red Hat, Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package automount

import (
	"fmt"
	"path/filepath"

	"github.com/devfile/devworkspace-operator/apis/controller/v1alpha1"
	"github.com/devfile/devworkspace-operator/pkg/constants"
	"github.com/devfile/devworkspace-operator/pkg/provision/sync"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
)

const hostKey = "host"
const certificateKey = "certificate"

const nameKey = "name"
const emailKey = "email"

const gitConfigName = "gitconfig"
const gitConfigLocation = "/etc/" + gitConfigName
const gitCredentialsConfigMapName = "devworkspace-gitconfig"

const credentialTemplate = `[credential]
    helper = store --file %s

`

const gitServerTemplate = `[http "%s"]
    sslCAInfo = %s

`

const defaultGitServerTemplate = `[http]
    sslCAInfo = %s

`

// provisionGitConfig takes care of mounting a gitconfig into a devworkspace.
//	It does so by:
//		1. Fill out the gitconfig with any necessary information:
//			a. Git user credentials if specified
//			b. Git server credentials if specified
//		2. Creating and mounting a gitconfig config map to /etc/gitconfig with the above information
func provisionGitConfig(api sync.ClusterAPI, namespace string, userMountPath string) (*v1alpha1.PodAdditions, error) {
	podAdditions, gitconfig, err := constructGitConfig(api, namespace, userMountPath)
	if err != nil {
		return podAdditions, nil
	}

	configMapPodAdditions, err := mountGitConfigMap(gitCredentialsConfigMapName, namespace, api, gitconfig)
	if err != nil {
		return configMapPodAdditions, err
	}
	podAdditions.Volumes = append(podAdditions.Volumes, configMapPodAdditions.Volumes...)
	podAdditions.VolumeMounts = append(podAdditions.VolumeMounts, configMapPodAdditions.VolumeMounts...)

	return podAdditions, nil
}

// constructGitConfig constructs the gitconfig and adds any relevant user and server credentials
func constructGitConfig(api sync.ClusterAPI, namespace string, userMountPath string) (*v1alpha1.PodAdditions, string, error) {
	gitconfig := ""

	if userMountPath != "" {
		// Initialize the credentials template to point to the local https credentials set by the user
		gitconfig = fmt.Sprintf(credentialTemplate, filepath.Join(userMountPath, gitCredentialsName))
	}

	podAdditions, gitconfig, err := constructGitConfigCertificates(api, namespace, gitconfig)
	if err != nil {
		return nil, "", err
	}

	gitconfig, err = constructGitUserConfig(api, namespace, gitconfig)
	if err != nil {
		return nil, "", err
	}
	return podAdditions, gitconfig, nil
}

// constructGitUserConfig finds one configmap with the label: constants.DevWorkspaceGitUserLabel: "true" and then uses those to
// construct a partial gitconfig with any relevant user credentials added
func constructGitUserConfig(api sync.ClusterAPI, namespace string, gitconfig string) (string, error) {
	configmap := &corev1.ConfigMapList{}
	err := api.Client.List(api.Ctx, configmap, k8sclient.InNamespace(namespace), k8sclient.MatchingLabels{
		constants.DevWorkspaceGitUserLabel: "true",
	})
	if err != nil {
		return "", err
	}
	if len(configmap.Items) > 1 {
		return "", &FatalError{fmt.Errorf("only one configmap in the namespace %s can have the label controller.devfile.io/git-user-credential", namespace)}
	}
	if len(configmap.Items) == 1 {
		cm := configmap.Items[0]
		name, nameFound := cm.Data[nameKey]
		email, emailFound := cm.Data[emailKey]

		if nameFound {
			gitconfig = gitconfig + fmt.Sprintf("user.name=%s\n", name)
		}

		if emailFound {
			gitconfig = gitconfig + fmt.Sprintf("user.email=%s\n", email)
		}
	}
	return gitconfig, nil
}

// constructGitConfigCertificates finds ALL configmaps with the label: constants.DevWorkspaceGitTLSLabel: "true" and then uses those to
// construct a partial gitconfig with any relevant server credentials added
func constructGitConfigCertificates(api sync.ClusterAPI, namespace string, gitconfig string) (*v1alpha1.PodAdditions, string, error) {
	configmap := &corev1.ConfigMapList{}
	err := api.Client.List(api.Ctx, configmap, k8sclient.InNamespace(namespace), k8sclient.MatchingLabels{
		constants.DevWorkspaceGitTLSLabel: "true",
	})
	if err != nil {
		return nil, "", err
	}

	podAdditions := &v1alpha1.PodAdditions{}

	defaultTlsAlreadyFound := false
	for _, cm := range configmap.Items {

		host, hostFound := cm.Data[hostKey]
		_, certFound := cm.Data[certificateKey]
		mountPath, mountPathFound := cm.Annotations[constants.DevWorkspaceMountPathAnnotation]

		if !mountPathFound {
			return nil, "", &FatalError{fmt.Errorf("could not find mount path in configmap %s", cm.Name)}
		}

		if !certFound {
			// If we aren't given the certificate data then we can't actually add the sslCAInfo
			return nil, "", &FatalError{fmt.Errorf("could not find certificate field in configmap %s", cm.Name)}
		}

		if !hostFound {
			// If there is already a configmap that does not specify the host we have to fail early because
			// we aren't able to tell what certificate we should use by default
			if defaultTlsAlreadyFound {
				return nil, "", &FatalError{fmt.Errorf("multiple git tls credentials do not have host specified")}
			} else {
				defaultTlsAlreadyFound = true
				gitconfig = gitconfig + fmt.Sprintf(defaultGitServerTemplate, filepath.Join(mountPath, certificateKey))
			}
		}

		// Create the tls certificate volume
		podAdditions.Volumes = append(podAdditions.Volumes, GetAutoMountVolumeWithConfigMap(cm.Name))

		// Create the tls certificate volume mount and point it to the defaultMountPath
		podAdditions.VolumeMounts = append(podAdditions.VolumeMounts, GetAutoMountConfigMapVolumeMount(mountPath, cm.Name))

		if hostFound || !defaultTlsAlreadyFound {
			gitconfig = gitconfig + fmt.Sprintf(gitServerTemplate, host, filepath.Join(mountPath, certificateKey))
		}
	}
	return podAdditions, gitconfig, nil
}

// mountGitConfigMap mounts the gitconfig to /etc/gitconfig in all devworkspaces in the given namespace.
//   It does so by:
//		1. Creating the configmap that stores the gitconfig if it does not exist
//		2. Adding the new config map volume and volume mount to the pod additions
func mountGitConfigMap(configMapName, namespace string, api sync.ClusterAPI, credentialsGitConfig string) (*v1alpha1.PodAdditions, error) {
	podAdditions := &v1alpha1.PodAdditions{}

	// Create the configmap that stores the gitconfig
	err := createOrUpdateGitConfigMap(configMapName, namespace, credentialsGitConfig, api)
	if err != nil {
		return nil, err
	}

	// Create the volume for the configmap
	podAdditions.Volumes = append(podAdditions.Volumes, GetAutoMountVolumeWithConfigMap(configMapName))

	// Create the gitconfig volume mount and set it's location as /etc/gitconfig so it's automatically picked up by git
	gitConfigMapVolumeMount := getGitConfigMapVolumeMount(configMapName)
	podAdditions.VolumeMounts = append(podAdditions.VolumeMounts, gitConfigMapVolumeMount)

	return podAdditions, nil
}

func getGitConfigMapVolumeMount(configMapName string) corev1.VolumeMount {
	gitConfigMapVolumeMount := GetAutoMountConfigMapVolumeMount(gitConfigLocation, configMapName)
	gitConfigMapVolumeMount.SubPath = gitConfigName
	gitConfigMapVolumeMount.ReadOnly = false
	return gitConfigMapVolumeMount
}

func createOrUpdateGitConfigMap(configMapName string, namespace string, config string, api sync.ClusterAPI) error {
	configMap := getGitConfigMap(configMapName, namespace, config)
	if err := api.Client.Create(api.Ctx, configMap); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		existingCfg, err := getClusterGitConfigMap(configMapName, namespace, api)
		if err != nil {
			return err
		}
		if existingCfg == nil {
			return nil
		}
		configMap.ResourceVersion = existingCfg.ResourceVersion
		err = api.Client.Update(api.Ctx, configMap)
		if err != nil {
			return err
		}

	}
	return nil
}

func getClusterGitConfigMap(configMapName string, namespace string, api sync.ClusterAPI) (*corev1.ConfigMap, error) {
	configMap := &corev1.ConfigMap{}
	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      configMapName,
	}
	err := api.Client.Get(api.Ctx, namespacedName, configMap)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return configMap, nil
}

func getGitConfigMap(configMapName string, namespace string, config string) *corev1.ConfigMap {
	gitConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/defaultName":         "git-config-secret",
				"app.kubernetes.io/part-of":             "devworkspace-operator",
				"controller.devfile.io/watch-configmap": "true",
			},
		},
		Data: map[string]string{
			gitConfigName: config,
		},
	}

	return gitConfigMap
}
