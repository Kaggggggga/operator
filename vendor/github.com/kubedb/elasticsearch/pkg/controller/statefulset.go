package controller

import (
	"fmt"

	"github.com/appscode/go/log"
	"github.com/appscode/go/types"
	"github.com/appscode/kutil"
	app_util "github.com/appscode/kutil/apps/v1"
	core_util "github.com/appscode/kutil/core/v1"
	api "github.com/kubedb/apimachinery/apis/kubedb/v1alpha1"
	"github.com/kubedb/apimachinery/pkg/eventer"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientsetscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/reference"
	mona "kmodules.xyz/monitoring-agent-api/api/v1"
)

const (
	CONFIG_MOUNT_PATH = "/elasticsearch/custom-config"
)

func (c *Controller) ensureStatefulSet(
	elasticsearch *api.Elasticsearch,
	pvcSpec *core.PersistentVolumeClaimSpec,
	resources core.ResourceRequirements,
	statefulSetName string,
	labels map[string]string,
	replicas int32,
	envList []core.EnvVar,
	isClient bool,
) (kutil.VerbType, error) {

	elasticsearchVersion, err := c.ExtClient.ElasticsearchVersions().Get(string(elasticsearch.Spec.Version), metav1.GetOptions{})
	if err != nil {
		return kutil.VerbUnchanged, err
	}

	if err := c.checkStatefulSet(elasticsearch, statefulSetName); err != nil {
		return kutil.VerbUnchanged, err
	}

	statefulSetMeta := metav1.ObjectMeta{
		Name:      statefulSetName,
		Namespace: elasticsearch.Namespace,
	}

	ref, rerr := reference.GetReference(clientsetscheme.Scheme, elasticsearch)
	if rerr != nil {
		return kutil.VerbUnchanged, rerr
	}

	searchGuard := string(elasticsearch.Spec.Version[0])

	statefulSet, vt, err := app_util.CreateOrPatchStatefulSet(c.Client, statefulSetMeta, func(in *apps.StatefulSet) *apps.StatefulSet {
		in.Labels = core_util.UpsertMap(labels, elasticsearch.OffshootLabels())
		in.Annotations = elasticsearch.Spec.PodTemplate.Controller.Annotations
		in.ObjectMeta = core_util.EnsureOwnerReference(in.ObjectMeta, ref)

		in.Spec.Replicas = types.Int32P(replicas)

		in.Spec.ServiceName = c.GoverningService
		in.Spec.Selector = &metav1.LabelSelector{
			MatchLabels: core_util.UpsertMap(labels, elasticsearch.OffshootSelectors()),
		}
		in.Spec.Template.Labels = core_util.UpsertMap(labels, elasticsearch.OffshootSelectors())
		in.Spec.Template.Annotations = elasticsearch.Spec.PodTemplate.Annotations
		in.Spec.Template.Spec.InitContainers = core_util.UpsertContainers(
			in.Spec.Template.Spec.InitContainers,
			append(
				[]core.Container{
					{
						Name:            "init-sysctl",
						Image:           "busybox",
						ImagePullPolicy: core.PullIfNotPresent,
						Command:         []string{"sysctl", "-w", "vm.max_map_count=262144"},
						SecurityContext: &core.SecurityContext{
							Privileged: types.BoolP(true),
						},
					},
				},
				elasticsearch.Spec.PodTemplate.Spec.InitContainers...,
			),
		)
		in.Spec.Template.Spec.Containers = core_util.UpsertContainer(
			in.Spec.Template.Spec.Containers,
			core.Container{
				Name:  api.ResourceSingularElasticsearch,
				Image: elasticsearchVersion.Spec.DB.Image,
				SecurityContext: &core.SecurityContext{
					Privileged: types.BoolP(false),
					Capabilities: &core.Capabilities{
						Add: []core.Capability{"IPC_LOCK", "SYS_RESOURCE"},
					},
				},
				Resources: resources,
			})
		in = upsertEnv(in, elasticsearch, envList)
		in = upsertUserEnv(in, elasticsearch)
		in = upsertPort(in, isClient)
		in = upsertCustomConfig(in, elasticsearch)

		in.Spec.Template.Spec.NodeSelector = elasticsearch.Spec.PodTemplate.Spec.NodeSelector
		in.Spec.Template.Spec.Affinity = elasticsearch.Spec.PodTemplate.Spec.Affinity
		if elasticsearch.Spec.PodTemplate.Spec.SchedulerName != "" {
			in.Spec.Template.Spec.SchedulerName = elasticsearch.Spec.PodTemplate.Spec.SchedulerName
		}
		in.Spec.Template.Spec.Tolerations = elasticsearch.Spec.PodTemplate.Spec.Tolerations
		in.Spec.Template.Spec.ImagePullSecrets = elasticsearch.Spec.PodTemplate.Spec.ImagePullSecrets
		in.Spec.Template.Spec.PriorityClassName = elasticsearch.Spec.PodTemplate.Spec.PriorityClassName
		in.Spec.Template.Spec.Priority = elasticsearch.Spec.PodTemplate.Spec.Priority
		in.Spec.Template.Spec.SecurityContext = elasticsearch.Spec.PodTemplate.Spec.SecurityContext

		if isClient {
			in = c.upsertMonitoringContainer(in, elasticsearch, elasticsearchVersion)
			in = upsertDatabaseSecret(in, elasticsearch.Spec.DatabaseSecret.SecretName, searchGuard)
		}

		in = upsertCertificate(in, elasticsearch.Spec.CertificateSecret.SecretName, isClient, elasticsearch.Spec.EnableSSL)
		in = upsertDataVolume(in, elasticsearch.Spec.StorageType, pvcSpec)
		in.Spec.UpdateStrategy.Type = apps.RollingUpdateStatefulSetStrategyType

		return in
	})

	if err != nil {
		return kutil.VerbUnchanged, err
	}

	if vt == kutil.VerbCreated || vt == kutil.VerbPatched {
		// Check StatefulSet Pod status
		if err := c.CheckStatefulSetPodStatus(statefulSet); err != nil {
			if ref, rerr := reference.GetReference(clientsetscheme.Scheme, elasticsearch); rerr == nil {
				c.recorder.Eventf(
					ref,
					core.EventTypeWarning,
					eventer.EventReasonFailedToStart,
					`Failed to be running after StatefulSet %v. Reason: %v`,
					vt,
					err,
				)
			}
			return kutil.VerbUnchanged, err
		}

		if ref, rerr := reference.GetReference(clientsetscheme.Scheme, elasticsearch); rerr == nil {
			c.recorder.Eventf(
				ref,
				core.EventTypeNormal,
				eventer.EventReasonSuccessful,
				"Successfully %v StatefulSet",
				vt,
			)
		}
	}
	return vt, nil
}

func (c *Controller) CheckStatefulSetPodStatus(statefulSet *apps.StatefulSet) error {
	err := core_util.WaitUntilPodRunningBySelector(
		c.Client,
		statefulSet.Namespace,
		statefulSet.Spec.Selector,
		int(types.Int32(statefulSet.Spec.Replicas)),
	)
	if err != nil {
		return err
	}
	return nil
}

func getHeapSizeForNode(val int64) int64 {
	ret := val / 100
	return ret * 80
}

func (c *Controller) ensureClientNode(elasticsearch *api.Elasticsearch) (kutil.VerbType, error) {
	statefulSetName := elasticsearch.OffshootName()
	clientNode := elasticsearch.Spec.Topology.Client

	if clientNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", clientNode.Prefix, statefulSetName)
	}

	labels := elasticsearch.OffshootLabels()
	labels[NodeRoleClient] = "set"

	heapSize := int64(134217728) // 128mb
	if request, found := clientNode.Resources.Requests[core.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	envList := []core.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "NODE_DATA",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "MODE",
			Value: "client",
		},
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
	}

	replicas := int32(1)
	if clientNode.Replicas != nil {
		replicas = types.Int32(clientNode.Replicas)
	}

	return c.ensureStatefulSet(elasticsearch, clientNode.Storage, clientNode.Resources, statefulSetName, labels, replicas, envList, true)
}

func (c *Controller) ensureMasterNode(elasticsearch *api.Elasticsearch) (kutil.VerbType, error) {
	statefulSetName := elasticsearch.OffshootName()
	masterNode := elasticsearch.Spec.Topology.Master

	if masterNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", masterNode.Prefix, statefulSetName)
	}

	labels := elasticsearch.OffshootLabels()
	labels[NodeRoleMaster] = "set"

	heapSize := int64(134217728) // 128mb
	if request, found := masterNode.Resources.Requests[core.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	replicas := int32(1)
	if masterNode.Replicas != nil {
		replicas = types.Int32(masterNode.Replicas)
	}

	envList := []core.EnvVar{
		{
			Name:  "NODE_DATA",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "NODE_INGEST",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "HTTP_ENABLE",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "NUMBER_OF_MASTERS",
			Value: fmt.Sprintf("%v", (replicas/2)+1),
		},
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
	}

	return c.ensureStatefulSet(elasticsearch, masterNode.Storage, masterNode.Resources, statefulSetName, labels, replicas, envList, false)
}

func (c *Controller) ensureDataNode(elasticsearch *api.Elasticsearch) (kutil.VerbType, error) {
	statefulSetName := elasticsearch.OffshootName()
	dataNode := elasticsearch.Spec.Topology.Data

	if dataNode.Prefix != "" {
		statefulSetName = fmt.Sprintf("%v-%v", dataNode.Prefix, statefulSetName)
	}

	labels := elasticsearch.OffshootLabels()
	labels[NodeRoleData] = "set"

	heapSize := int64(134217728) // 128mb
	if request, found := dataNode.Resources.Requests[core.ResourceMemory]; found && request.Value() > 0 {
		heapSize = getHeapSizeForNode(request.Value())
	}

	envList := []core.EnvVar{
		{
			Name:  "NODE_MASTER",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "NODE_INGEST",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "HTTP_ENABLE",
			Value: fmt.Sprintf("%v", false),
		},
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
	}

	replicas := int32(1)
	if dataNode.Replicas != nil {
		replicas = types.Int32(dataNode.Replicas)
	}

	return c.ensureStatefulSet(elasticsearch, dataNode.Storage, dataNode.Resources, statefulSetName, labels, replicas, envList, false)
}

func (c *Controller) ensureCombinedNode(elasticsearch *api.Elasticsearch) (kutil.VerbType, error) {
	statefulSetName := elasticsearch.OffshootName()
	labels := elasticsearch.OffshootLabels()
	labels[NodeRoleClient] = "set"
	labels[NodeRoleMaster] = "set"
	labels[NodeRoleData] = "set"

	replicas := int32(1)
	if elasticsearch.Spec.Replicas != nil {
		replicas = types.Int32(elasticsearch.Spec.Replicas)
	}

	heapSize := int64(134217728) // 128mb
	if elasticsearch.Spec.Resources != nil {
		if request, found := elasticsearch.Spec.Resources.Requests[core.ResourceMemory]; found && request.Value() > 0 {
			heapSize = getHeapSizeForNode(request.Value())
		}
	}

	envList := []core.EnvVar{
		{
			Name:  "NUMBER_OF_MASTERS",
			Value: fmt.Sprintf("%v", (replicas/2)+1),
		},
		{
			Name:  "MODE",
			Value: "client",
		},
		{
			Name:  "ES_JAVA_OPTS",
			Value: fmt.Sprintf("-Xms%v -Xmx%v", heapSize, heapSize),
		},
	}

	var pvcSpec core.PersistentVolumeClaimSpec
	var resources core.ResourceRequirements
	if elasticsearch.Spec.Storage != nil {
		pvcSpec = *elasticsearch.Spec.Storage
	}
	if elasticsearch.Spec.Resources != nil {
		resources = *elasticsearch.Spec.Resources
	}
	return c.ensureStatefulSet(elasticsearch, &pvcSpec, resources, statefulSetName, labels, replicas, envList, true)
}

func (c *Controller) checkStatefulSet(elasticsearch *api.Elasticsearch, name string) error {
	elasticsearchName := elasticsearch.OffshootName()
	// SatatefulSet for Elasticsearch database
	statefulSet, err := c.Client.AppsV1().StatefulSets(elasticsearch.Namespace).Get(name, metav1.GetOptions{})
	if err != nil {
		if kerr.IsNotFound(err) {
			return nil
		} else {
			return err
		}
	}

	if statefulSet.Labels[api.LabelDatabaseKind] != api.ResourceKindElasticsearch ||
		statefulSet.Labels[api.LabelDatabaseName] != elasticsearchName {
		return fmt.Errorf(`intended statefulSet "%v" already exists`, name)
	}

	return nil
}

func upsertEnv(statefulSet *apps.StatefulSet, elasticsearch *api.Elasticsearch, envs []core.EnvVar) *apps.StatefulSet {
	envList := []core.EnvVar{
		{
			Name:  "CLUSTER_NAME",
			Value: elasticsearch.Name,
		},
		{
			Name: "NODE_NAME",
			ValueFrom: &core.EnvVarSource{
				FieldRef: &core.ObjectFieldSelector{
					FieldPath: "metadata.name",
				},
			},
		},
		{
			Name:  "DISCOVERY_SERVICE",
			Value: elasticsearch.MasterServiceName(),
		},
		{
			Name:  "SSL_ENABLE",
			Value: fmt.Sprintf("%v", elasticsearch.Spec.EnableSSL),
		},
		{
			Name: "KEY_PASS",
			ValueFrom: &core.EnvVarSource{
				SecretKeyRef: &core.SecretKeySelector{
					LocalObjectReference: core.LocalObjectReference{
						Name: elasticsearch.Spec.CertificateSecret.SecretName,
					},
					Key: "key_pass",
				},
			},
		},
		{
			Name:  "SEARCHGUARD_DISABLED",
			Value: fmt.Sprintf("%v", elasticsearch.SearchGuardDisabled()),
		},
	}

	envList = append(envList, envs...)

	// To do this, Upsert Container first
	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			statefulSet.Spec.Template.Spec.Containers[i].Env = core_util.UpsertEnvVars(container.Env, envList...)
			return statefulSet
		}
	}

	return statefulSet
}

// upsertUserEnv add/overwrite env from user provided env in crd spec
func upsertUserEnv(statefulSet *apps.StatefulSet, elasticsearch *api.Elasticsearch) *apps.StatefulSet {
	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			statefulSet.Spec.Template.Spec.Containers[i].Env = core_util.UpsertEnvVars(container.Env, elasticsearch.Spec.PodTemplate.Spec.Env...)
			return statefulSet
		}
	}
	return statefulSet
}

func upsertPort(statefulSet *apps.StatefulSet, isClient bool) *apps.StatefulSet {

	getPorts := func() []core.ContainerPort {
		portList := []core.ContainerPort{
			{
				Name:          ElasticsearchNodePortName,
				ContainerPort: ElasticsearchNodePort,
				Protocol:      core.ProtocolTCP,
			},
		}
		if isClient {
			portList = append(portList, core.ContainerPort{
				Name:          ElasticsearchRestPortName,
				ContainerPort: ElasticsearchRestPort,
				Protocol:      core.ProtocolTCP,
			})
		}

		return portList
	}

	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			statefulSet.Spec.Template.Spec.Containers[i].Ports = getPorts()
			return statefulSet
		}
	}

	return statefulSet
}

func (c *Controller) upsertMonitoringContainer(statefulSet *apps.StatefulSet, elasticsearch *api.Elasticsearch, elasticsearchVersion *api.ElasticsearchVersion) *apps.StatefulSet {
	if elasticsearch.GetMonitoringVendor() == mona.VendorPrometheus {
		container := core.Container{
			Name: "exporter",
			Args: append([]string{
				"export",
				fmt.Sprintf("--address=:%d", api.PrometheusExporterPortNumber),
				fmt.Sprintf("--enable-analytics=%v", c.EnableAnalytics),
			}, c.LoggerOptions.ToFlags()...),
			Image:           elasticsearchVersion.Spec.Exporter.Image,
			ImagePullPolicy: core.PullIfNotPresent,
			Ports: []core.ContainerPort{
				{
					Name:          api.PrometheusExporterPortName,
					Protocol:      core.ProtocolTCP,
					ContainerPort: int32(api.PrometheusExporterPortNumber),
				},
			},
			VolumeMounts: []core.VolumeMount{
				{
					Name:      "secret",
					MountPath: ExporterSecretPath,
				},
			},
		}
		containers := statefulSet.Spec.Template.Spec.Containers
		containers = core_util.UpsertContainer(containers, container)
		statefulSet.Spec.Template.Spec.Containers = containers

		volume := core.Volume{
			Name: "secret",
			VolumeSource: core.VolumeSource{
				Secret: &core.SecretVolumeSource{
					SecretName: elasticsearch.Spec.DatabaseSecret.SecretName,
				},
			},
		}
		volumes := statefulSet.Spec.Template.Spec.Volumes
		volumes = core_util.UpsertVolume(volumes, volume)
		statefulSet.Spec.Template.Spec.Volumes = volumes

		// this environment variable will be used to determine url scheme in exporter
		envList := []core.EnvVar{
			{
				Name:  "USE_SSL",
				Value: fmt.Sprintf("%v", elasticsearch.Spec.EnableSSL),
			},
		}

		for i, container := range statefulSet.Spec.Template.Spec.Containers {
			if container.Name == "exporter" {
				statefulSet.Spec.Template.Spec.Containers[i].Env = core_util.UpsertEnvVars(container.Env, envList...)
				return statefulSet
			}
		}
	}
	return statefulSet
}

func upsertCertificate(statefulSet *apps.StatefulSet, secretName string, isClientNode, isEnalbeSSL bool) *apps.StatefulSet {
	addCertVolume := func() *core.SecretVolumeSource {
		svs := &core.SecretVolumeSource{
			SecretName: secretName,
			Items: []core.KeyToPath{
				{
					Key:  "root.jks",
					Path: "root.jks",
				},
				{
					Key:  "node.jks",
					Path: "node.jks",
				},
			},
		}

		if isEnalbeSSL {
			svs.Items = append(svs.Items, core.KeyToPath{
				Key:  "client.jks",
				Path: "client.jks",
			})
		}

		if isClientNode {
			svs.Items = append(svs.Items, core.KeyToPath{
				Key:  "sgadmin.jks",
				Path: "sgadmin.jks",
			})
		}
		return svs
	}

	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			volumeMount := core.VolumeMount{
				Name:      "certs",
				MountPath: "/elasticsearch/config/certs",
			}
			volumeMounts := container.VolumeMounts
			volumeMounts = core_util.UpsertVolumeMount(volumeMounts, volumeMount)
			statefulSet.Spec.Template.Spec.Containers[i].VolumeMounts = volumeMounts

			volume := core.Volume{
				Name: "certs",
				VolumeSource: core.VolumeSource{
					Secret: addCertVolume(),
				},
			}
			volumes := statefulSet.Spec.Template.Spec.Volumes
			volumes = core_util.UpsertVolume(volumes, volume)
			statefulSet.Spec.Template.Spec.Volumes = volumes
			return statefulSet
		}
	}
	return statefulSet
}

func upsertDatabaseSecret(statefulSet *apps.StatefulSet, secretName string, searchGuard string) *apps.StatefulSet {
	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			volumeMount := core.VolumeMount{
				Name:      "sgconfig",
				MountPath: fmt.Sprintf("/elasticsearch/plugins/search-guard-%v/sgconfig", searchGuard),
			}
			volumeMounts := container.VolumeMounts
			volumeMounts = core_util.UpsertVolumeMount(volumeMounts, volumeMount)
			statefulSet.Spec.Template.Spec.Containers[i].VolumeMounts = volumeMounts

			volume := core.Volume{
				Name: "sgconfig",
				VolumeSource: core.VolumeSource{
					Secret: &core.SecretVolumeSource{
						SecretName: secretName,
					},
				},
			}
			volumes := statefulSet.Spec.Template.Spec.Volumes
			volumes = core_util.UpsertVolume(volumes, volume)
			statefulSet.Spec.Template.Spec.Volumes = volumes
			return statefulSet
		}
	}
	return statefulSet
}

func upsertDataVolume(statefulSet *apps.StatefulSet, st api.StorageType, pvcSpec *core.PersistentVolumeClaimSpec) *apps.StatefulSet {
	for i, container := range statefulSet.Spec.Template.Spec.Containers {
		if container.Name == api.ResourceSingularElasticsearch {
			volumeMount := core.VolumeMount{
				Name:      "data",
				MountPath: "/data",
			}
			volumeMounts := container.VolumeMounts
			volumeMounts = core_util.UpsertVolumeMount(volumeMounts, volumeMount)
			statefulSet.Spec.Template.Spec.Containers[i].VolumeMounts = volumeMounts

			if st == api.StorageTypeEphemeral {
				ed := core.EmptyDirVolumeSource{}
				if pvcSpec != nil {
					if sz, found := pvcSpec.Resources.Requests[core.ResourceStorage]; found {
						ed.SizeLimit = &sz
					}
				}
				statefulSet.Spec.Template.Spec.Volumes = core_util.UpsertVolume(
					statefulSet.Spec.Template.Spec.Volumes,
					core.Volume{
						Name: "data",
						VolumeSource: core.VolumeSource{
							EmptyDir: &ed,
						},
					})
			} else {
				if len(pvcSpec.AccessModes) == 0 {
					pvcSpec.AccessModes = []core.PersistentVolumeAccessMode{
						core.ReadWriteOnce,
					}
					log.Infof(`Using "%v" as AccessModes in "%v"`, core.ReadWriteOnce, pvcSpec)
				}

				claim := core.PersistentVolumeClaim{
					ObjectMeta: metav1.ObjectMeta{
						Name: "data",
					},
					Spec: *pvcSpec,
				}
				if pvcSpec.StorageClassName != nil {
					claim.Annotations = map[string]string{
						"volume.beta.kubernetes.io/storage-class": *pvcSpec.StorageClassName,
					}
				}
				statefulSet.Spec.VolumeClaimTemplates = core_util.UpsertVolumeClaim(statefulSet.Spec.VolumeClaimTemplates, claim)
			}

			return statefulSet
		}
	}
	return statefulSet
}

func upsertCustomConfig(statefulSet *apps.StatefulSet, elasticsearch *api.Elasticsearch) *apps.StatefulSet {
	if elasticsearch.Spec.ConfigSource != nil {
		for i, container := range statefulSet.Spec.Template.Spec.Containers {
			if container.Name == api.ResourceSingularElasticsearch {
				configVolumeMount := core.VolumeMount{
					Name:      "custom-config",
					MountPath: CONFIG_MOUNT_PATH,
				}
				volumeMounts := container.VolumeMounts
				volumeMounts = core_util.UpsertVolumeMount(volumeMounts, configVolumeMount)
				statefulSet.Spec.Template.Spec.Containers[i].VolumeMounts = volumeMounts

				configVolume := core.Volume{
					Name:         "custom-config",
					VolumeSource: *elasticsearch.Spec.ConfigSource,
				}

				volumes := statefulSet.Spec.Template.Spec.Volumes
				volumes = core_util.UpsertVolume(volumes, configVolume)
				statefulSet.Spec.Template.Spec.Volumes = volumes
				break
			}
		}
	}
	return statefulSet
}
