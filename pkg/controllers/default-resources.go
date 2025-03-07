/*
Copyright 2022.

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
	hypdeployment "github.com/jnpacker/hypershift-deployment-controller/api/v1alpha1"
	hyp "github.com/openshift/hypershift/api/v1alpha1"
	"github.com/openshift/hypershift/cmd/infra/aws"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const ReleaseImage = "quay.io/openshift-release-dev/ocp-release:4.9.15-x86_64"

func ScafoldHostedCluster(hyd *hypdeployment.HypershiftDeployment, hcs *hyp.HostedClusterSpec) *hyp.HostedCluster {

	return &hyp.HostedCluster{
		ObjectMeta: v1.ObjectMeta{
			Name:      hyd.Name,
			Namespace: hyd.Namespace,
		},
		Spec: *hcs,
	}
}

// Creates an instance of ServicePublishingStrategyMapping
func spsMap(service hyp.ServiceType, psType hyp.PublishingStrategyType) hyp.ServicePublishingStrategyMapping {

	return hyp.ServicePublishingStrategyMapping{
		Service: service,
		ServicePublishingStrategy: hyp.ServicePublishingStrategy{
			Type: hyp.PublishingStrategyType(psType),
		},
	}
}

func ScafoldHostedClusterSpec(hyd *hypdeployment.HypershiftDeployment, infraOut *aws.CreateInfraOutput) {

	volSize := resource.MustParse("4Gi")
	//releaseImage, _ := version.LookupDefaultOCPVersion()

	if hyd.Spec.HostedClusterSpec == nil {
		hyd.Spec.HostedClusterSpec =
			&hyp.HostedClusterSpec{
				ControllerAvailabilityPolicy: hyp.SingleReplica,
				Etcd: hyp.EtcdSpec{
					Managed: &hyp.ManagedEtcdSpec{
						Storage: hyp.ManagedEtcdStorageSpec{
							PersistentVolume: &hyp.PersistentVolumeEtcdStorageSpec{
								Size: &volSize,
							},
							Type: hyp.PersistentVolumeEtcdStorage,
						},
					},
					ManagementType: hyp.Managed,
				},
				FIPS: false,

				//IssuerURL: iamOut.IssuerURL,
				Networking: hyp.ClusterNetworking{
					ServiceCIDR: "172.31.0.0/16",
					PodCIDR:     "10.132.0.0/14",
					MachineCIDR: "", //This is overwritten below
					NetworkType: hyp.OpenShiftSDN,
				},
				// This is specific AWS
				Platform: hyp.PlatformSpec{
					Type: hyp.AWSPlatform,
					AWS: &hyp.AWSPlatformSpec{
						ControlPlaneOperatorCreds: corev1.LocalObjectReference{Name: hyd.Name + "-cpo-creds"},
						KubeCloudControllerCreds:  corev1.LocalObjectReference{Name: hyd.Name + "-cloud-ctrl-creds"},
						NodePoolManagementCreds:   corev1.LocalObjectReference{Name: hyd.Name + "-node-mgmt-creds"},
						EndpointAccess:            hyp.Public,
						//Roles:                     iamOut.Roles,
					},
				},
				// Defaults for all platforms
				PullSecret: corev1.LocalObjectReference{Name: hyd.Name + "-pull-secret"},
				Release: hyp.Release{
					Image: ReleaseImage, //.DownloadURL,
				},
				Services: []hyp.ServicePublishingStrategyMapping{
					spsMap(hyp.APIServer, hyp.LoadBalancer),
					spsMap(hyp.OAuthServer, hyp.Route),
					spsMap(hyp.OIDC, hyp.S3),
					spsMap(hyp.Konnectivity, hyp.Route),
					spsMap(hyp.Ignition, hyp.Route),
				},
			}
	}

	hyd.Spec.HostedClusterSpec.DNS = *scafoldDnsSpec(infraOut)
	hyd.Spec.HostedClusterSpec.InfraID = hyd.Spec.InfraID
	hyd.Spec.HostedClusterSpec.Networking.MachineCIDR = infraOut.ComputeCIDR
	hyd.Spec.HostedClusterSpec.Platform.AWS.Region = hyd.Spec.Infrastructure.Platform.AWS.Region
	hyd.Spec.HostedClusterSpec.Platform.AWS.CloudProviderConfig = scafoldCloudProviderConfig(infraOut)

}

func scafoldDnsSpec(infraOut *aws.CreateInfraOutput) *hyp.DNSSpec {
	return &hyp.DNSSpec{
		BaseDomain:    infraOut.BaseDomain,
		PrivateZoneID: infraOut.PrivateZoneID,
		PublicZoneID:  infraOut.PublicZoneID,
	}
}

func scafoldCloudProviderConfig(infraOut *aws.CreateInfraOutput) *hyp.AWSCloudProviderConfig {
	return &hyp.AWSCloudProviderConfig{
		Subnet: &hyp.AWSResourceReference{
			ID: &infraOut.PrivateSubnetID,
		},
		VPC:  infraOut.VPCID,
		Zone: infraOut.Zone,
	}
}

func ScafoldNodePoolSpec(hyd *hypdeployment.HypershiftDeployment, infraOut *aws.CreateInfraOutput) {

	nodeCount := int32(2)

	if len(hyd.Spec.NodePools) == 0 {
		hyd.Spec.NodePools = []*hypdeployment.HypershiftNodePools{
			&hypdeployment.HypershiftNodePools{
				Name: hyd.Name,
				Spec: hyp.NodePoolSpec{
					ClusterName: hyd.Name,
					Management: hyp.NodePoolManagement{
						AutoRepair: false,
						Replace: &hyp.ReplaceUpgrade{
							RollingUpdate: &hyp.RollingUpdate{
								MaxSurge:       &intstr.IntOrString{IntVal: 1},
								MaxUnavailable: &intstr.IntOrString{IntVal: 0},
							},
							Strategy: hyp.UpgradeStrategyRollingUpdate,
						},
						UpgradeType: hyp.UpgradeTypeReplace,
					},
					NodeCount: &nodeCount,
					Platform: hyp.NodePoolPlatform{
						//AWS is added below
						Type: hyp.AWSPlatform,
					},
					Release: hyp.Release{
						Image: ReleaseImage, //.DownloadURL,,
					},
				},
			},
		}
	}

	for _, np := range hyd.Spec.NodePools {

		if np.Spec.ClusterName != hyd.Name {
			np.Spec.ClusterName = hyd.Name
		}
		if np.Spec.Platform.AWS == nil {
			np.Spec.Platform.AWS = scafoldAWSNodePoolPlatform(infraOut)
		}
		if np.Spec.Platform.AWS.InstanceProfile == "" {
			np.Spec.Platform.AWS.InstanceProfile = hyd.Spec.InfraID + "-worker"
		}
		if np.Spec.Platform.AWS.Subnet == nil {
			np.Spec.Platform.AWS.Subnet = &hyp.AWSResourceReference{
				ID: &infraOut.PrivateSubnetID,
			}
		}
		if np.Spec.Platform.AWS.SecurityGroups == nil {
			np.Spec.Platform.AWS.SecurityGroups = []hyp.AWSResourceReference{
				hyp.AWSResourceReference{
					ID: &infraOut.SecurityGroupID,
				},
			}
		}
	}
}

func scafoldAWSNodePoolPlatform(infraOut *aws.CreateInfraOutput) *hyp.AWSNodePoolPlatform {
	volSize := int64(35)

	return &hyp.AWSNodePoolPlatform{
		InstanceType: "t3.large",
		RootVolume: &hyp.Volume{
			Size: volSize,
			Type: "gp3",
		},
	}
}

func ScafoldNodePool(namespace string, infraId string, np *hypdeployment.HypershiftNodePools) *hyp.NodePool {

	return &hyp.NodePool{
		ObjectMeta: v1.ObjectMeta{
			Name:      np.Name,
			Namespace: namespace,
			Labels: map[string]string{
				AutoInfraLabelName: infraId,
			},
		},
		Spec: np.Spec,
	}
}
