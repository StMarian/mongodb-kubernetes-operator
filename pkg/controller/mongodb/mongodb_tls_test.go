package mongodb

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	mdbv1 "github.com/mongodb/mongodb-kubernetes-operator/pkg/apis/mongodb/v1"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/automationconfig"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/client"
	mdbClient "github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/client"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/configmap"
	"github.com/mongodb/mongodb-kubernetes-operator/pkg/kube/secret"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestStatefulSet_IsCorrectlyConfiguredWithTLS(t *testing.T) {
	mdb := newTestReplicaSetWithTLS()
	mgr := client.NewManager(&mdb)

	err := createTLSSecretAndConfigMap(mgr.GetClient(), mdb)
	assert.NoError(t, err)

	r := newReconciler(mgr, mockManifestProvider(mdb.Spec.Version))
	res, err := r.Reconcile(reconcile.Request{NamespacedName: types.NamespacedName{Namespace: mdb.Namespace, Name: mdb.Name}})
	assertReconciliationSuccessful(t, res, err)

	sts := appsv1.StatefulSet{}
	err = mgr.GetClient().Get(context.TODO(), types.NamespacedName{Name: mdb.Name, Namespace: mdb.Namespace}, &sts)
	assert.NoError(t, err)

	// Assert that all TLS volumes have been added.
	assert.Len(t, sts.Spec.Template.Spec.Volumes, 5)
	assert.Contains(t, sts.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "tls-ca",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: mdb.Spec.Security.TLS.CaConfigMap.Name,
				},
			},
		},
	})
	assert.Contains(t, sts.Spec.Template.Spec.Volumes, corev1.Volume{
		Name: "tls-secret",
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: mdb.TLSOperatorSecretNamespacedName().Name,
			},
		},
	})

	tlsSecretVolumeMount := corev1.VolumeMount{
		Name:      "tls-secret",
		ReadOnly:  true,
		MountPath: tlsOperatorSecretMountPath,
	}
	tlsCAVolumeMount := corev1.VolumeMount{
		Name:      "tls-ca",
		ReadOnly:  true,
		MountPath: tlsCAMountPath,
	}

	assert.Len(t, sts.Spec.Template.Spec.InitContainers, 1)

	agentContainer := sts.Spec.Template.Spec.Containers[0]
	assert.Contains(t, agentContainer.VolumeMounts, tlsSecretVolumeMount)
	assert.Contains(t, agentContainer.VolumeMounts, tlsCAVolumeMount)

	mongodbContainer := sts.Spec.Template.Spec.Containers[1]
	assert.Contains(t, mongodbContainer.VolumeMounts, tlsSecretVolumeMount)
	assert.Contains(t, mongodbContainer.VolumeMounts, tlsCAVolumeMount)
}

func TestAutomationConfig_IsCorrectlyConfiguredWithTLS(t *testing.T) {
	createAC := func(mdb mdbv1.MongoDB) automationconfig.AutomationConfig {
		client := mdbClient.NewClient(client.NewManager(&mdb).GetClient())
		err := createTLSSecretAndConfigMap(client, mdb)
		assert.NoError(t, err)

		manifest, err := mockManifestProvider(mdb.Spec.Version)()
		assert.NoError(t, err)
		versionConfig := manifest.BuildsForVersion(mdb.Spec.Version)

		tlsModification, err := getTLSConfigModification(client, mdb)
		assert.NoError(t, err)

		ac, err := buildAutomationConfig(mdb, versionConfig, automationconfig.AutomationConfig{}, tlsModification)
		assert.NoError(t, err)

		return ac
	}

	t.Run("With TLS disabled", func(t *testing.T) {
		mdb := newTestReplicaSet()
		ac := createAC(mdb)

		assert.Equal(t, automationconfig.TLS{
			CAFilePath:            "",
			ClientCertificateMode: automationconfig.ClientCertificateModeOptional,
		}, ac.TLS)

		for _, process := range ac.Processes {
			assert.Equal(t, automationconfig.MongoDBTLS{
				Mode: automationconfig.TLSModeDisabled,
			}, process.Args26.Net.TLS)
		}
	})

	t.Run("With TLS enabled, during rollout", func(t *testing.T) {
		mdb := newTestReplicaSetWithTLS()
		ac := createAC(mdb)

		assert.Equal(t, automationconfig.TLS{
			CAFilePath:            "",
			ClientCertificateMode: automationconfig.ClientCertificateModeOptional,
		}, ac.TLS)

		for _, process := range ac.Processes {
			assert.Equal(t, automationconfig.MongoDBTLS{
				Mode: automationconfig.TLSModeDisabled,
			}, process.Args26.Net.TLS)
		}
	})

	t.Run("With TLS enabled and required, rollout completed", func(t *testing.T) {
		mdb := newTestReplicaSetWithTLS()
		mdb.Annotations[tlsRolledOutAnnotationKey] = "true"
		ac := createAC(mdb)

		assert.Equal(t, automationconfig.TLS{
			CAFilePath:            tlsCAMountPath + tlsCACertName,
			ClientCertificateMode: automationconfig.ClientCertificateModeOptional,
		}, ac.TLS)

		for _, process := range ac.Processes {
			operatorSecretFileName := tlsOperatorSecretFileName("CERT", "KEY")

			assert.Equal(t, automationconfig.MongoDBTLS{
				Mode:                               automationconfig.TLSModeRequired,
				PEMKeyFile:                         tlsOperatorSecretMountPath + operatorSecretFileName,
				CAFile:                             tlsCAMountPath + tlsCACertName,
				AllowConnectionsWithoutCertificate: true,
			}, process.Args26.Net.TLS)
		}
	})

	t.Run("With TLS enabled and optional, rollout completed", func(t *testing.T) {
		mdb := newTestReplicaSetWithTLS()
		mdb.Annotations[tlsRolledOutAnnotationKey] = "true"
		mdb.Spec.Security.TLS.Optional = true
		ac := createAC(mdb)

		assert.Equal(t, automationconfig.TLS{
			CAFilePath:            tlsCAMountPath + tlsCACertName,
			ClientCertificateMode: automationconfig.ClientCertificateModeOptional,
		}, ac.TLS)

		for _, process := range ac.Processes {
			operatorSecretFileName := tlsOperatorSecretFileName("CERT", "KEY")

			assert.Equal(t, automationconfig.MongoDBTLS{
				Mode:                               automationconfig.TLSModePreferred,
				PEMKeyFile:                         tlsOperatorSecretMountPath + operatorSecretFileName,
				CAFile:                             tlsCAMountPath + tlsCACertName,
				AllowConnectionsWithoutCertificate: true,
			}, process.Args26.Net.TLS)
		}
	})
}

func TestTLSOperatorSecret(t *testing.T) {
	t.Run("Secret is created if it doesn't exist", func(t *testing.T) {
		mdb := newTestReplicaSetWithTLS()
		client := mdbClient.NewClient(client.NewManager(&mdb).GetClient())
		err := createTLSSecretAndConfigMap(client, mdb)
		assert.NoError(t, err)

		_, err = getTLSConfigModification(client, mdb)
		assert.NoError(t, err)

		// Operator-managed secret should have been created and contain the
		// concatenated certificate and key.
		certificateKey, err := secret.ReadKey(client, tlsOperatorSecretFileName("CERT", "KEY"), mdb.TLSOperatorSecretNamespacedName())
		assert.NoError(t, err)
		assert.Equal(t, "CERTKEY", certificateKey)
	})

	t.Run("Secret is updated if it already exists", func(t *testing.T) {
		mdb := newTestReplicaSetWithTLS()
		client := mdbClient.NewClient(client.NewManager(&mdb).GetClient())
		err := createTLSSecretAndConfigMap(client, mdb)
		assert.NoError(t, err)

		// Create operator-managed secret
		s := secret.Builder().
			SetName(mdb.TLSOperatorSecretNamespacedName().Name).
			SetNamespace(mdb.TLSOperatorSecretNamespacedName().Namespace).
			SetField(tlsOperatorSecretFileName("", ""), "").
			Build()
		err = client.CreateSecret(s)
		assert.NoError(t, err)

		_, err = getTLSConfigModification(client, mdb)
		assert.NoError(t, err)

		// Operator-managed secret should have been updated with the concatenated
		// certificate and key.
		certificateKey, err := secret.ReadKey(client, tlsOperatorSecretFileName("CERT", "KEY"), mdb.TLSOperatorSecretNamespacedName())
		assert.NoError(t, err)
		assert.Equal(t, "CERTKEY", certificateKey)
	})
}

func createTLSSecretAndConfigMap(c k8sClient.Client, mdb mdbv1.MongoDB) error {
	s := secret.Builder().
		SetName(mdb.Spec.Security.TLS.CertificateKeySecret.Name).
		SetNamespace(mdb.Namespace).
		SetField("tls.crt", "CERT").
		SetField("tls.key", "KEY").
		Build()

	err := c.Create(context.TODO(), &s)
	if err != nil {
		return err
	}

	configMap := configmap.Builder().
		SetName(mdb.Spec.Security.TLS.CaConfigMap.Name).
		SetNamespace(mdb.Namespace).
		SetField("ca.crt", "CERT").
		Build()

	err = c.Create(context.TODO(), &configMap)
	if err != nil {
		return err
	}

	return nil
}
