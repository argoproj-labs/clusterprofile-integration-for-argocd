package main

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/v3/common"
	appv1alpha1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testClusterName          = "test-cluster"
	testNamespace            = "default"
	testServer               = "https://test-cluster.example.com"
	testSecretName           = "cluster-test-cluster"
	testProviderName         = "secretreader"
	testProviderCommand      = "/plugins/secretreader/bin/secretreader-plugin"
	unconfiguredProviderName = "not-configured"
	builtinGCPProviderName   = "argo-cd-builtin-gcp"
	clusterProfileAPIVersion = "multicluster.x-k8s.io/v1alpha1"
	clusterProfileKind       = "ClusterProfile"
	hubContextName           = "hub"
	spokeContextName         = "spoke"
	trueValue                = "true"
	argocdNamespace          = "argocd"
	environmentLabel         = "environment"
	teamLabel                = "team"
	productionValue          = "production"
	stagingValue             = "staging"
	platformTeamValue        = "platform"
	managedByLabel           = "app.example.com/managed-by"
	teamANamespace           = "team-a"
	teamBNamespace           = "team-b"
	testClusterProfileUID    = "cluster-profile-uid"
)

type deleteConflictClient struct {
	client.Client
	deleteOptions client.DeleteOptions
}

type updateCountingClient struct {
	client.Client
	updates int
}

type patchConflictClient struct {
	client.Client
}

func (c *updateCountingClient) Update(
	ctx context.Context,
	object client.Object,
	options ...client.UpdateOption,
) error {
	c.updates++
	return c.Client.Update(ctx, object, options...)
}

func (c *deleteConflictClient) Delete(
	_ context.Context,
	object client.Object,
	options ...client.DeleteOption,
) error {
	c.deleteOptions = *(&client.DeleteOptions{}).ApplyOptions(options)
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "secrets"},
		object.GetName(),
		errors.New("simulated cached-read race"),
	)
}

func (c *patchConflictClient) Patch(
	_ context.Context,
	object client.Object,
	_ client.Patch,
	_ ...client.PatchOption,
) error {
	return apierrors.NewConflict(
		schema.GroupResource{Resource: "secrets"},
		object.GetName(),
		errors.New("simulated cached-read patch race"),
	)
}

func newBuiltinProviderClusterProfile(labels map[string]string) *clusterinventory.ClusterProfile {
	return &clusterinventory.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			Labels:    labels,
		},
		Status: clusterinventory.ClusterProfileStatus{
			AccessProviders: []clusterinventory.AccessProvider{
				{
					Name: builtinGCPProviderName,
					Cluster: clientcmdv1.Cluster{
						Server: testServer,
					},
				},
			},
		},
	}
}

func newCustomProviderClusterProfile(labels map[string]string) *clusterinventory.ClusterProfile {
	return &clusterinventory.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testClusterName,
			Namespace: testNamespace,
			UID:       testClusterProfileUID,
			Labels:    labels,
		},
		Status: clusterinventory.ClusterProfileStatus{
			AccessProviders: []clusterinventory.AccessProvider{
				{
					Name: testProviderName,
					Cluster: clientcmdv1.Cluster{
						Server: testServer,
					},
				},
			},
		},
	}
}

func clusterProfileOwnerReference(name string, uid types.UID) metav1.OwnerReference {
	controller := true
	blockOwnerDeletion := false
	return metav1.OwnerReference{
		APIVersion:         clusterProfileAPIVersion,
		Kind:               clusterProfileKind,
		Name:               name,
		UID:                uid,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

func newControlledSecret(
	clusterProfile *clusterinventory.ClusterProfile,
	uid types.UID,
) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            testSecretName,
			Namespace:       clusterProfile.Namespace,
			UID:             uid,
			ResourceVersion: "42",
			Labels: map[string]string{
				clusterProfileNameKey:     clusterProfile.Name,
				common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
			},
			OwnerReferences: []metav1.OwnerReference{
				clusterProfileOwnerReference(clusterProfile.Name, clusterProfile.UID),
			},
		},
		Data: map[string][]byte{
			secretDataNameKey:   []byte(clusterProfile.Name),
			secretDataServerKey: []byte(testServer),
			secretDataConfigKey: []byte(`{"previous":true}`),
		},
	}
}

func seedStaleClusterSecretData(secret *corev1.Secret) {
	secret.Data[secretDataNameKey] = []byte("stale-name")
	secret.Data[secretDataServerKey] = []byte("https://stale.example.com")
	secret.Data["project"] = []byte("stale-project")
	secret.Data["namespaces"] = []byte("stale-namespace")
	secret.Data["clusterResources"] = []byte(trueValue)
	secret.Data["shard"] = []byte("99")
	secret.Data["stale-auth-material"] = []byte("must-be-removed")
}

func requireExactTestClusterSecretData(t *testing.T, secret *corev1.Secret) appv1alpha1.ClusterConfig {
	t.Helper()

	require.Equal(t, map[string][]byte{
		secretDataNameKey:   []byte(testClusterName),
		secretDataServerKey: []byte(testServer),
		secretDataConfigKey: secret.Data[secretDataConfigKey],
	}, secret.Data)
	require.Empty(t, secret.StringData)

	var config appv1alpha1.ClusterConfig
	require.NoError(t, json.Unmarshal(secret.Data[secretDataConfigKey], &config))
	return config
}

func assertDeletePreconditions(t *testing.T, conflictClient *deleteConflictClient, secret *corev1.Secret) {
	t.Helper()

	require.NotNil(t, conflictClient.deleteOptions.Preconditions)
	require.NotNil(t, conflictClient.deleteOptions.Preconditions.UID)
	assert.Equal(t, secret.UID, *conflictClient.deleteOptions.Preconditions.UID)
	require.NotNil(t, conflictClient.deleteOptions.Preconditions.ResourceVersion)
	assert.Equal(t, secret.ResourceVersion, *conflictClient.deleteOptions.Preconditions.ResourceVersion)
}

func assertSecretPreserved(t *testing.T, before, after *corev1.Secret) {
	t.Helper()

	assert.Equal(t, before.UID, after.UID)
	assert.Equal(t, before.ResourceVersion, after.ResourceVersion)
	assert.Equal(t, before.Labels, after.Labels)
	assert.Equal(t, before.Data, after.Data)
	assert.Equal(t, before.OwnerReferences, after.OwnerReferences)
}

func TestRenderSecretValidatesAccessProviders(t *testing.T) {
	reconciler := &ClusterProfileReconciler{}

	for _, cloudProvider := range []string{"aws", "azure", "gcp"} {
		t.Run("supports built-in "+cloudProvider, func(t *testing.T) {
			profile := newBuiltinProviderClusterProfile(nil)
			profile.Status.AccessProviders[0].Name = "argo-cd-builtin-" + cloudProvider

			rendered, err := reconciler.renderSecret(profile)

			require.NoError(t, err)
			var config appv1alpha1.ClusterConfig
			require.NoError(t, json.Unmarshal(rendered.data[secretDataConfigKey], &config))
			require.NotNil(t, config.ExecProviderConfig)
			assert.Equal(t, "argocd-k8s-auth", config.ExecProviderConfig.Command)
			assert.Equal(t, []string{cloudProvider}, config.ExecProviderConfig.Args)
		})
	}

	for _, providerName := range []string{
		"argo-cd-builtin-",
		"argo-cd-builtin-GCP",
		"argo-cd-builtin-unknown",
	} {
		t.Run("rejects unsupported built-in "+providerName, func(t *testing.T) {
			profile := newBuiltinProviderClusterProfile(nil)
			profile.Status.AccessProviders[0].Name = providerName

			_, err := reconciler.renderSecret(profile)

			require.ErrorContains(t, err, "unsupported built-in access provider")
			require.ErrorContains(t, err, providerName)
		})
	}

	t.Run("rejects an empty built-in cluster server", func(t *testing.T) {
		profile := newBuiltinProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Cluster.Server = " \t"

		_, err := reconciler.renderSecret(profile)

		require.ErrorContains(t, err, "has an empty cluster server")
	})

	t.Run("rejects an empty custom-provider cluster server", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Cluster.Server = ""
		customReconciler := &ClusterProfileReconciler{
			ClusterProfileProviderFile: writeProvidersFile(t),
		}
		require.NoError(t, customReconciler.loadClusterProfileProviderFile())

		_, err := customReconciler.renderSecret(profile)

		require.ErrorContains(t, err, "has an empty cluster server")
	})
}

func TestRenderSecretPreservesClusterConnectionFields(t *testing.T) {
	connection := clientcmdv1.Cluster{
		Server:                   testServer,
		TLSServerName:            "api.internal.example.com",
		InsecureSkipTLSVerify:    true,
		CertificateAuthorityData: []byte("test-ca-data"),
		ProxyURL:                 "socks5://proxy.example.com:1080",
		DisableCompression:       true,
	}

	assertConnection := func(t *testing.T, rendered *renderedSecret) {
		t.Helper()

		assert.Equal(t, []byte(testServer), rendered.data[secretDataServerKey])
		var config appv1alpha1.ClusterConfig
		require.NoError(t, json.Unmarshal(rendered.data[secretDataConfigKey], &config))
		assert.Equal(t, connection.InsecureSkipTLSVerify, config.Insecure)
		assert.Equal(t, connection.TLSServerName, config.ServerName)
		assert.Equal(t, connection.CertificateAuthorityData, config.CAData)
		assert.Equal(t, connection.ProxyURL, config.ProxyUrl)
		assert.Equal(t, connection.DisableCompression, config.DisableCompression)
	}

	t.Run("built-in provider", func(t *testing.T) {
		profile := newBuiltinProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Cluster = connection

		rendered, err := (&ClusterProfileReconciler{}).renderSecret(profile)

		require.NoError(t, err)
		assertConnection(t, rendered)
	})

	t.Run("custom provider", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Cluster = connection
		reconciler := &ClusterProfileReconciler{
			ClusterProfileProviderFile: writeProvidersFile(t),
		}
		require.NoError(t, reconciler.loadClusterProfileProviderFile())

		rendered, err := reconciler.renderSecret(profile)

		require.NoError(t, err)
		assertConnection(t, rendered)
	})
}

func writeProvidersFile(t *testing.T) string {
	t.Helper()

	execConfig := map[string]any{
		"apiVersion":         "client.authentication.k8s.io/v1",
		"command":            testProviderCommand,
		"provideClusterInfo": true,
	}
	providerConfig := map[string]any{
		"providers": []map[string]any{
			{
				secretDataNameKey: testProviderName,
				"execConfig":      execConfig,
			},
		},
	}
	data, err := json.Marshal(providerConfig)
	require.NoError(t, err)

	return writeProviderConfigFile(t, data)
}

func writeProviderConfigFile(t *testing.T, data []byte) string {
	t.Helper()

	file, err := os.CreateTemp(t.TempDir(), "providers.json")
	require.NoError(t, err)
	_, err = file.Write(data)
	require.NoError(t, err)
	require.NoError(t, file.Close())

	return file.Name()
}

func TestBuildRESTConfig(t *testing.T) {
	t.Run("uses the selected context and applies controller defaults", func(t *testing.T) {
		config := clientcmdapi.Config{
			Clusters: map[string]*clientcmdapi.Cluster{
				hubContextName:   {Server: "https://hub.example.com"},
				spokeContextName: {Server: "https://spoke.example.com"},
			},
			Contexts: map[string]*clientcmdapi.Context{
				hubContextName:   {Cluster: hubContextName},
				spokeContextName: {Cluster: spokeContextName},
			},
			CurrentContext: spokeContextName,
		}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: hubContextName}
		clientConfig := clientcmd.NewNonInteractiveClientConfig(config, "", overrides, nil)

		restConfig, err := buildRESTConfig(clientConfig)

		require.NoError(t, err)
		assert.Equal(t, "https://hub.example.com", restConfig.Host)
		version := common.GetVersion()
		assert.Equal(t,
			cliName+"/"+version.Version+" ("+version.Platform+")",
			restConfig.UserAgent,
		)
		assert.Equal(t, appv1alpha1.K8sClientConfigQPS, restConfig.QPS)
		assert.Equal(t, appv1alpha1.K8sClientConfigBurst, restConfig.Burst)
		assert.Equal(t, appv1alpha1.K8sServerSideTimeout, restConfig.Timeout)
		assert.NotNil(t, restConfig.Transport)
		assert.Empty(t, restConfig.TLSClientConfig)
	})

	t.Run("returns an error for an invalid client configuration", func(t *testing.T) {
		clientConfig := clientcmd.NewNonInteractiveClientConfig(
			clientcmdapi.Config{},
			"",
			&clientcmd.ConfigOverrides{},
			nil,
		)

		restConfig, err := buildRESTConfig(clientConfig)

		assert.Nil(t, restConfig)
		require.Error(t, err)
		assert.ErrorContains(t, err, "failed to load Kubernetes REST config")
	})
}

func TestCacheSyncReadiness(t *testing.T) {
	readiness := &cacheSyncReadiness{}
	require.ErrorContains(t, readiness.Check(nil), "manager cache has not synced")
	assert.False(t, readiness.NeedLeaderElection())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- readiness.Start(ctx)
	}()
	require.Eventually(t, func() bool {
		return readiness.Check(nil) == nil
	}, time.Second, time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	require.ErrorContains(t, readiness.Check(nil), "manager cache has not synced")
}

func TestGeneratedClusterProfileMetadata(t *testing.T) {
	t.Run("preserves name labels at the raw limit and bounds the next byte", func(t *testing.T) {
		atLimit := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("a", content.LabelValueMaxLength),
		}}
		overLimit := atLimit.DeepCopy()
		overLimit.Name += "a"

		assert.Equal(t, atLimit.Name, clusterProfileNameLabelValue(atLimit))
		bounded := clusterProfileNameLabelValue(overLimit)
		assert.Len(t, bounded, content.LabelValueMaxLength)
		assert.Empty(t, content.IsLabelValue(bounded))
		assert.Equal(t, bounded, clusterProfileNameLabelValue(overLimit))
	})

	t.Run("preserves Secret names at the raw limit and bounds longer valid profiles", func(t *testing.T) {
		atLimit := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("a", content.DNS1123SubdomainMaxLength-len("cluster-")),
		}}
		overLimit := atLimit.DeepCopy()
		overLimit.Name += "a"
		maxProfileName := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("m", content.DNS1123SubdomainMaxLength),
		}}

		assert.Len(t, "cluster-"+atLimit.Name, content.DNS1123SubdomainMaxLength)
		assert.Len(t, "cluster-"+overLimit.Name, content.DNS1123SubdomainMaxLength+1)
		assert.Equal(t, "cluster-"+atLimit.Name, clusterProfileSecretName(atLimit))
		for _, clusterProfile := range []*clusterinventory.ClusterProfile{overLimit, maxProfileName} {
			bounded := clusterProfileSecretName(clusterProfile)
			assert.Len(t, bounded, content.DNS1123SubdomainMaxLength)
			assert.Empty(t, content.IsDNS1123Subdomain(bounded))
			assert.Equal(t, bounded, clusterProfileSecretName(clusterProfile))
		}
	})

	t.Run("keeps bounded values valid when truncation lands on punctuation", func(t *testing.T) {
		nameProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("a", 29) + "-" + strings.Repeat("b", 40),
		}}
		secretProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("a", 211) + "." + strings.Repeat("b", 34),
		}}

		labelValue := clusterProfileNameLabelValue(nameProfile)
		secretName := clusterProfileSecretName(secretProfile)
		assert.Empty(t, content.IsLabelValue(labelValue))
		assert.Empty(t, content.IsDNS1123Subdomain(secretName))
		assert.NotContains(t, labelValue, "-_")
		assert.NotContains(t, secretName, ".-")
	})

	t.Run("does not collide for long names with the same readable prefix", func(t *testing.T) {
		prefix := strings.Repeat("a", 245)
		first := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{Name: prefix + "b"}}
		second := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{Name: prefix + "c"}}

		assert.NotEqual(t, clusterProfileSecretName(first), clusterProfileSecretName(second))
		assert.NotEqual(t, clusterProfileNameLabelValue(first), clusterProfileNameLabelValue(second))
	})

	t.Run("keeps bounded encodings disjoint from every raw encoding", func(t *testing.T) {
		longSecretProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("s", 246),
		}}
		previousBoundedSecretName := "cluster-" + strings.Repeat("s", 212) +
			"-c487d7cd89959dbc1df6f5deec5584b5"
		rawSecretCollisionProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.TrimPrefix(previousBoundedSecretName, "cluster-"),
		}}
		assert.Equal(t, previousBoundedSecretName, clusterProfileSecretName(rawSecretCollisionProfile))
		assert.Equal(t,
			"clusterprofile-"+strings.Repeat("s", 205)+"-c487d7cd89959dbc1df6f5deec5584b5",
			clusterProfileSecretName(longSecretProfile),
		)
		assert.NotEqual(t, clusterProfileSecretName(longSecretProfile), clusterProfileSecretName(rawSecretCollisionProfile))
		assert.NotRegexp(t, `^cluster-`, clusterProfileSecretName(longSecretProfile))
		assert.Regexp(t, `^cluster-`, clusterProfileSecretName(rawSecretCollisionProfile))

		longNameProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("l", 64),
		}}
		rawNameCollisionProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: strings.Repeat("l", 30) + "-6714e95219c67c4cda7eeff21b662ca5",
		}}
		assert.Equal(t, rawNameCollisionProfile.Name, clusterProfileNameLabelValue(rawNameCollisionProfile))
		assert.Equal(t,
			strings.Repeat("l", 30)+"_6714e95219c67c4cda7eeff21b662ca5",
			clusterProfileNameLabelValue(longNameProfile),
		)
		assert.NotEqual(t,
			clusterProfileNameLabelValue(longNameProfile),
			clusterProfileNameLabelValue(rawNameCollisionProfile),
		)
		assert.Contains(t, clusterProfileNameLabelValue(longNameProfile), "_")
		assert.NotContains(t, clusterProfileNameLabelValue(rawNameCollisionProfile), "_")
	})

	t.Run("retains short metadata values verbatim", func(t *testing.T) {
		clusterProfile := &clusterinventory.ClusterProfile{ObjectMeta: metav1.ObjectMeta{
			Name: testClusterName,
		}}

		assert.Equal(t, testSecretName, clusterProfileSecretName(clusterProfile))
		assert.Equal(t, testClusterName, clusterProfileNameLabelValue(clusterProfile))
	})
}

func TestLongClusterProfileSecretLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterinventory.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	clusterProfile := newBuiltinProviderClusterProfile(nil)
	clusterProfile.Name = strings.Repeat("a", 246)
	clusterProfile.Namespace = argocdNamespace
	clusterProfile.UID = types.UID(testClusterProfileUID)
	secretName := clusterProfileSecretName(clusterProfile)
	r := &ClusterProfileReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
		Log:    logr.Discard(),
		Scheme: scheme,
	}
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	createdSecret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: argocdNamespace,
		Name:      secretName,
	}, createdSecret))
	assert.True(t, metav1.IsControlledBy(createdSecret, clusterProfile))
	assert.Equal(t, clusterProfile.Name, string(createdSecret.Data[secretDataNameKey]))
	assert.Equal(t, clusterProfile.Name, createdSecret.Annotations[clusterProfileNameKey])

	storedProfile := &clusterinventory.ClusterProfile{}
	require.NoError(t, r.Get(context.Background(), req.NamespacedName, storedProfile))
	storedProfile.Status.AccessProviders = nil
	require.NoError(t, r.Update(context.Background(), storedProfile))
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(r.Get(context.Background(), types.NamespacedName{
		Namespace: argocdNamespace,
		Name:      secretName,
	}, &corev1.Secret{})))

	require.NoError(t, r.Get(context.Background(), req.NamespacedName, storedProfile))
	storedProfile.Status.AccessProviders = newBuiltinProviderClusterProfile(nil).Status.AccessProviders
	require.NoError(t, r.Update(context.Background(), storedProfile))
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	restoredSecret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Namespace: argocdNamespace,
		Name:      secretName,
	}, restoredSecret))
	assert.Equal(t, createdSecret.Name, restoredSecret.Name)
	assert.True(t, metav1.IsControlledBy(restoredSecret, storedProfile))
}

func TestAccessProviderFingerprint(t *testing.T) {
	providerA := &clusterinventory.AccessProvider{
		Name: testProviderName,
		Cluster: clientcmdv1.Cluster{
			Server: testServer,
			Extensions: []clientcmdv1.NamedExtension{
				{
					Name: "z.example.io/config",
					Extension: runtime.RawExtension{
						Raw: []byte(`{ "nested": { "b": 2, "a": 1 } }`),
					},
				},
				{
					Name: "a.example.io/config",
					Extension: runtime.RawExtension{
						Raw: []byte(`{"enabled":true}`),
					},
				},
			},
		},
	}
	providerB := providerA.DeepCopy()
	providerB.Cluster.Extensions = []clientcmdv1.NamedExtension{
		{
			Name: "a.example.io/config",
			Extension: runtime.RawExtension{
				Raw: []byte("{\n  \"enabled\": true\n}"),
			},
		},
		{
			Name: "z.example.io/config",
			Extension: runtime.RawExtension{
				Raw: []byte(`{"nested":{"a":1,"b":2}}`),
			},
		},
	}
	fingerprintA, err := fingerprintAccessProvider(providerA)
	require.NoError(t, err)
	fingerprintB, err := fingerprintAccessProvider(providerB)
	require.NoError(t, err)

	assert.Equal(t, fingerprintA, fingerprintB)
	assert.Equal(t,
		"v1:sha256:011e9131b524671b1fdf3087ef3e6e9ccceac035a5cc54939cd2934fb6e4c19a",
		fingerprintA,
	)
	assert.True(t, validFingerprint(fingerprintA))

	providerB.Cluster.Server = "https://changed.example.com"
	changedProvider, err := fingerprintAccessProvider(providerB)
	require.NoError(t, err)
	assert.NotEqual(t, fingerprintA, changedProvider)

	for _, invalid := range []string{
		"",
		"v2:sha256:" + strings.Repeat("0", 64),
		fingerprintPrefix + strings.Repeat("0", 63),
		fingerprintPrefix + strings.Repeat("G", 64),
		fingerprintPrefix + strings.Repeat("A", 64),
	} {
		assert.False(t, validFingerprint(invalid), invalid)
	}
}

func TestEffectiveAccessProviderSelection(t *testing.T) {
	profile := &clusterinventory.ClusterProfile{
		Status: clusterinventory.ClusterProfileStatus{
			CredentialProviders: []clusterinventory.CredentialProvider{
				{
					Name:    testProviderName,
					Cluster: clientcmdv1.Cluster{Server: "https://deprecated.example.com"},
				},
				{
					Name:    "second",
					Cluster: clientcmdv1.Cluster{Server: "https://second.example.com"},
				},
			},
			AccessProviders: []clusterinventory.AccessProvider{
				{
					Name:    testProviderName,
					Cluster: clientcmdv1.Cluster{Server: testServer},
				},
			},
		},
	}
	config := &access.Config{Providers: []access.Provider{
		{Name: "second"},
		{Name: testProviderName},
	}}

	effective := effectiveAccessProviders(profile)
	require.Len(t, effective, 2)
	assert.Equal(t, testServer, effective[testProviderName].Cluster.Server)
	selected := selectCustomAccessProvider(profile, config)
	require.NotNil(t, selected)
	assert.Equal(t, "second", selected.Name)

	config.Providers = slices.Clone(config.Providers[1:])
	selected = selectCustomAccessProvider(profile, config)
	require.NotNil(t, selected)
	assert.Equal(t, testProviderName, selected.Name)
	assert.Equal(t, testServer, selected.Cluster.Server)
}

func TestRenderFailureRevocation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterinventory.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	type fixture struct {
		reconciler     *ClusterProfileReconciler
		request        reconcile.Request
		originalSecret *corev1.Secret
	}
	newFixture := func(t *testing.T, labels map[string]string) *fixture {
		t.Helper()
		clusterProfile := newCustomProviderClusterProfile(labels)
		baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build()
		reconciler := &ClusterProfileReconciler{
			Client:                     baseClient,
			Log:                        logr.Discard(),
			Scheme:                     scheme,
			ClusterProfileProviderFile: writeProvidersFile(t),
		}
		require.NoError(t, reconciler.loadClusterProfileProviderFile())
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}
		_, err := reconciler.Reconcile(context.Background(), request)
		require.NoError(t, err)
		secret := &corev1.Secret{}
		require.NoError(t, reconciler.Get(context.Background(), types.NamespacedName{
			Name: testSecretName, Namespace: testNamespace,
		}, secret))
		require.True(t, validFingerprint(secret.Annotations[secretAccessProviderFingerprintAnnotation]))
		require.True(t, validFingerprint(secret.Annotations[secretPayloadFingerprintAnnotation]))
		return &fixture{reconciler: reconciler, request: request, originalSecret: secret.DeepCopy()}
	}
	updateProfile := func(t *testing.T, fixture *fixture, mutate func(*clusterinventory.ClusterProfile)) {
		t.Helper()
		profile := &clusterinventory.ClusterProfile{}
		require.NoError(t, fixture.reconciler.Get(context.Background(), fixture.request.NamespacedName, profile))
		mutate(profile)
		require.NoError(t, fixture.reconciler.Update(context.Background(), profile))
	}
	getSecret := func(t *testing.T, fixture *fixture) *corev1.Secret {
		t.Helper()
		secret := &corev1.Secret{}
		require.NoError(t, fixture.reconciler.Get(context.Background(), types.NamespacedName{
			Name: testSecretName, Namespace: testNamespace,
		}, secret))
		return secret
	}
	requireSecretDeleted := func(t *testing.T, fixture *fixture) {
		t.Helper()
		err := fixture.reconciler.Get(context.Background(), types.NamespacedName{
			Name: testSecretName, Namespace: testNamespace,
		}, &corev1.Secret{})
		assert.True(t, apierrors.IsNotFound(err), err)
	}

	t.Run("retains last-known-good data during a local provider configuration outage", func(t *testing.T) {
		fixture := newFixture(t, map[string]string{environmentLabel: productionValue})
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.ErrorContains(t, err, "AccessProviders not initialized")
		preserved := getSecret(t, fixture)
		assert.Equal(t, fixture.originalSecret.ResourceVersion, preserved.ResourceVersion)
		assert.Equal(t, fixture.originalSecret.Data, preserved.Data)
		assert.Equal(t, fixture.originalSecret.Labels, preserved.Labels)
		assert.Equal(t, fixture.originalSecret.Annotations, preserved.Annotations)
	})

	t.Run("retains provider A when unrelated provider B is added during an outage", func(t *testing.T) {
		fixture := newFixture(t, nil)
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders = append(profile.Status.AccessProviders,
				clusterinventory.AccessProvider{
					Name:    "unrelated",
					Cluster: clientcmdv1.Cluster{Server: "https://unrelated.example.com"},
				})
		})
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		preserved := getSecret(t, fixture)
		assert.Equal(t, fixture.originalSecret.Data, preserved.Data)
		assert.Equal(t, fixture.originalSecret.Annotations, preserved.Annotations)
	})

	t.Run("revokes credentials when the selected provider changes", func(t *testing.T) {
		fixture := newFixture(t, nil)
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders = []clusterinventory.AccessProvider{
				{
					Name:    unconfiguredProviderName,
					Cluster: clientcmdv1.Cluster{Server: "https://replacement.example.com"},
				},
			}
		})
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		requireSecretDeleted(t, fixture)
	})

	t.Run("updates ApplicationSet labels without re-rendering credentials", func(t *testing.T) {
		fixture := newFixture(t, map[string]string{
			environmentLabel: productionValue,
			"obsolete":       "remove-me",
		})
		originalProviderFingerprint :=
			fixture.originalSecret.Annotations[secretAccessProviderFingerprintAnnotation]
		originalPayloadFingerprint :=
			fixture.originalSecret.Annotations[secretPayloadFingerprintAnnotation]
		secretWithExternalAnnotation := getSecret(t, fixture)
		secretWithExternalAnnotation.Annotations["example.com/retained"] = trueValue
		require.NoError(t, fixture.reconciler.Update(context.Background(), secretWithExternalAnnotation))
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Labels = map[string]string{
				environmentLabel: stagingValue,
				teamLabel:        platformTeamValue,
			}
		})
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		updated := getSecret(t, fixture)
		assert.Equal(t, fixture.originalSecret.Data, updated.Data)
		assert.Equal(t, fixture.originalSecret.OwnerReferences, updated.OwnerReferences)
		assert.Equal(t, stagingValue, updated.Labels[environmentLabel])
		assert.Equal(t, platformTeamValue, updated.Labels[teamLabel])
		assert.NotContains(t, updated.Labels, "obsolete")
		assert.Equal(t, common.LabelValueSecretTypeCluster, updated.Labels[common.LabelKeySecretType])
		assert.Equal(t, testClusterName, updated.Labels[clusterProfileNameKey])
		assert.Equal(t, trueValue, updated.Annotations["example.com/retained"])
		assert.Equal(t, originalProviderFingerprint,
			updated.Annotations[secretAccessProviderFingerprintAnnotation])
		assert.NotEqual(t, originalPayloadFingerprint,
			updated.Annotations[secretPayloadFingerprintAnnotation])
	})

	t.Run("retries when the metadata-only optimistic patch conflicts", func(t *testing.T) {
		fixture := newFixture(t, map[string]string{environmentLabel: productionValue})
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Labels[environmentLabel] = stagingValue
		})
		fixture.reconciler.AccessProviders = nil
		fixture.reconciler.Client = &patchConflictClient{Client: fixture.reconciler.Client}

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err))
		preserved := getSecret(t, fixture)
		assert.Equal(t, productionValue, preserved.Labels[environmentLabel])
		assert.Equal(t, fixture.originalSecret.Annotations, preserved.Annotations)
	})

	t.Run("preserves a payload changed by an external writer during an outage", func(t *testing.T) {
		fixture := newFixture(t, nil)
		secret := getSecret(t, fixture)
		secret.Data[secretDataServerKey] = []byte("https://external-writer.example.com")
		require.NoError(t, fixture.reconciler.Update(context.Background(), secret))
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		preserved := getSecret(t, fixture)
		assert.Equal(t, secret.ResourceVersion, preserved.ResourceVersion)
		assert.Equal(t, secret.Data, preserved.Data)
	})

	t.Run("preserves an unprovable payload after the stamped provider changes", func(t *testing.T) {
		fixture := newFixture(t, nil)
		secret := getSecret(t, fixture)
		secret.Data[secretDataServerKey] = []byte("https://external-writer.example.com")
		require.NoError(t, fixture.reconciler.Update(context.Background(), secret))
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		})
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		preserved := getSecret(t, fixture)
		assert.Equal(t, secret.ResourceVersion, preserved.ResourceVersion)
		assert.Equal(t, secret.Data, preserved.Data)
	})

	t.Run("preserves missing and unknown fingerprint formats conservatively", func(t *testing.T) {
		testCases := []struct {
			name   string
			mutate func(map[string]string)
		}{
			{
				name: "Secret without annotations",
				mutate: func(annotations map[string]string) {
					delete(annotations, secretAccessProviderFingerprintAnnotation)
					delete(annotations, secretPayloadFingerprintAnnotation)
				},
			},
			{
				name: "future access-provider fingerprint",
				mutate: func(annotations map[string]string) {
					annotations[secretAccessProviderFingerprintAnnotation] =
						"v2:sha256:" + strings.Repeat("0", 64)
				},
			},
			{
				name: "malformed payload fingerprint",
				mutate: func(annotations map[string]string) {
					annotations[secretPayloadFingerprintAnnotation] = "not-a-fingerprint"
				},
			},
		}
		for _, testCase := range testCases {
			t.Run(testCase.name, func(t *testing.T) {
				fixture := newFixture(t, nil)
				secret := getSecret(t, fixture)
				testCase.mutate(secret.Annotations)
				require.NoError(t, fixture.reconciler.Update(context.Background(), secret))
				updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
					profile.Status.AccessProviders[0].Name = unconfiguredProviderName
				})
				fixture.reconciler.AccessProviders = nil

				_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

				require.Error(t, err)
				preserved := getSecret(t, fixture)
				assert.Equal(t, secret.ResourceVersion, preserved.ResourceVersion)
				assert.Equal(t, secret.Data, preserved.Data)
			})
		}
	})

	t.Run("preserves a foreign Secret without partially adopting it", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		foreignSecret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testSecretName,
				Namespace: testNamespace,
				Labels:    map[string]string{"managed-by": "someone-else"},
			},
			Data: map[string][]byte{"external": []byte("value")},
		}
		before := foreignSecret.DeepCopy()
		reconciler := &ClusterProfileReconciler{
			Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, foreignSecret).Build(),
			Log:    logr.Discard(),
			Scheme: scheme,
		}
		request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(profile)}

		_, err := reconciler.Reconcile(context.Background(), request)

		require.Error(t, err)
		persisted := &corev1.Secret{}
		require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(foreignSecret), persisted))
		assert.Equal(t, before.OwnerReferences, persisted.OwnerReferences)
		assert.Equal(t, before.Labels, persisted.Labels)
		assert.Equal(t, before.Data, persisted.Data)
	})

	t.Run("uses UID and resourceVersion preconditions when obsolete deletion races", func(t *testing.T) {
		fixture := newFixture(t, nil)
		secret := getSecret(t, fixture)
		secret.UID = "fingerprinted-secret-uid"
		require.NoError(t, fixture.reconciler.Update(context.Background(), secret))
		updateProfile(t, fixture, func(profile *clusterinventory.ClusterProfile) {
			profile.Status.AccessProviders[0].Name = unconfiguredProviderName
		})
		conflictClient := &deleteConflictClient{Client: fixture.reconciler.Client}
		fixture.reconciler.Client = conflictClient
		fixture.reconciler.AccessProviders = nil

		_, err := fixture.reconciler.Reconcile(context.Background(), fixture.request)

		require.Error(t, err)
		assert.True(t, apierrors.IsConflict(err))
		assertDeletePreconditions(t, conflictClient, secret)
		assert.Equal(t, secret.UID, getSecret(t, fixture).UID)
	})

	t.Run("backfills fingerprints and preserves unrelated annotations", func(t *testing.T) {
		profile := newCustomProviderClusterProfile(nil)
		secret := newControlledSecret(profile, "existing-secret-uid")
		secret.Annotations = map[string]string{"example.com/retained": trueValue}
		reconciler := &ClusterProfileReconciler{
			Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(profile, secret).Build(),
			Log:                        logr.Discard(),
			Scheme:                     scheme,
			ClusterProfileProviderFile: writeProvidersFile(t),
		}
		require.NoError(t, reconciler.loadClusterProfileProviderFile())

		_, err := reconciler.Reconcile(context.Background(), reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(profile),
		})

		require.NoError(t, err)
		updated := &corev1.Secret{}
		require.NoError(t, reconciler.Get(context.Background(), client.ObjectKeyFromObject(secret), updated))
		assert.Equal(t, trueValue, updated.Annotations["example.com/retained"])
		assert.True(t, validFingerprint(updated.Annotations[secretAccessProviderFingerprintAnnotation]))
		assert.True(t, validFingerprint(updated.Annotations[secretPayloadFingerprintAnnotation]))
	})
}

func TestClusterProfileReconciler(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, clusterinventory.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	t.Run("Reconcile", func(t *testing.T) {
		t.Run("should create a secret when a new ClusterProfile is created", func(t *testing.T) {
			providersFile := writeProvidersFile(t)

			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
				Status: clusterinventory.ClusterProfileStatus{
					AccessProviders: []clusterinventory.AccessProvider{
						{
							Name: testProviderName,
							Cluster: clientcmdv1.Cluster{
								Server: testServer,
								Extensions: []clientcmdv1.NamedExtension{
									{
										Name: "client.authentication.k8s.io/exec",
										Extension: runtime.RawExtension{
											Raw: []byte(`{"clusterName":"test-cluster"}`),
										},
									},
								},
							},
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			assert.Equal(t, testSecretName, secret.Name)
			assert.Equal(t, testNamespace, secret.Namespace)
			assert.Equal(t, []metav1.OwnerReference{
				clusterProfileOwnerReference(testClusterName, ""),
			}, secret.OwnerReferences)
			assert.Equal(t, "cluster", secret.Labels["argocd.argoproj.io/secret-type"])
			assert.Equal(t, testClusterName, secret.Labels["argocd.argoproj.io/cluster-profile-name"])
			assert.Equal(t, testClusterName, secret.Annotations["argocd.argoproj.io/cluster-profile-name"])
			assert.Equal(t, testClusterName, string(secret.Data[secretDataNameKey]))
			assert.Equal(t, testServer, string(secret.Data[secretDataServerKey]))

			var configMap map[string]any
			require.NoError(t, json.Unmarshal(secret.Data[secretDataConfigKey], &configMap))
			execProviderConfig := configMap["execProviderConfig"].(map[string]any)
			assert.Equal(t, "client.authentication.k8s.io/v1", execProviderConfig["apiVersion"])
			assert.Equal(t, testProviderCommand, execProviderConfig["command"])
			assert.Equal(t, true, execProviderConfig["provideClusterInfo"])
			config := execProviderConfig["config"].(map[string]any)
			assert.Equal(t, testClusterName, config["clusterName"])
		})

		t.Run("should not retain profile-sourced args between reconciles", func(t *testing.T) {
			execConfig := map[string]any{
				"apiVersion": "client.authentication.k8s.io/v1",
				"command":    testProviderCommand,
			}
			providerConfig := map[string]any{
				"providers": []map[string]any{
					{
						secretDataNameKey:             testProviderName,
						"execConfig":                  execConfig,
						"profileSourcedCLIArgsPolicy": access.ProfileSourcedCLIArgsPolicyAppend,
					},
				},
			}
			data, err := json.Marshal(providerConfig)
			require.NoError(t, err)
			providersFile := writeProviderConfigFile(t, data)

			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
				Status: clusterinventory.ClusterProfileStatus{
					AccessProviders: []clusterinventory.AccessProvider{
						{
							Name: testProviderName,
							Cluster: clientcmdv1.Cluster{
								Server: testServer,
								Extensions: []clientcmdv1.NamedExtension{
									{
										Name: "clusterprofiles.multicluster.x-k8s.io/exec/additional-args",
										Extension: runtime.RawExtension{
											Raw: []byte(`["--cluster", "{{ .ClusterProfileName }}"]`),
										},
									},
								},
							},
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			var configMap map[string]any
			require.NoError(t, json.Unmarshal(secret.Data[secretDataConfigKey], &configMap))
			execProviderConfig := configMap["execProviderConfig"].(map[string]any)
			assert.Equal(t, []any{"--cluster", testClusterName}, execProviderConfig["args"])
		})

		t.Run("should not create a builtin secret from deprecated CredentialProviders", func(t *testing.T) {
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
				Status: clusterinventory.ClusterProfileStatus{
					CredentialProviders: []clusterinventory.CredentialProvider{
						{
							Name: builtinGCPProviderName,
							Cluster: clientcmdv1.Cluster{
								Server: testServer,
							},
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.Error(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			assert.True(t, apierrors.IsNotFound(err))
		})

		t.Run("should propagate ClusterProfile labels to the generated secret", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: productionValue,
				teamLabel:        platformTeamValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, platformTeamValue, secret.Labels[teamLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should protect controller-owned secret labels from ClusterProfile labels", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				common.LabelKeySecretType: "not-cluster",
				clusterProfileNameKey:     "other-name",
				environmentLabel:          productionValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should update secret labels when ClusterProfile labels change", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: stagingValue,
				teamLabel:        platformTeamValue,
			})
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			updatedClusterProfile := &clusterinventory.ClusterProfile{}
			err = r.Get(context.Background(), req.NamespacedName, updatedClusterProfile)
			require.NoError(t, err)
			updatedClusterProfile.Labels = map[string]string{
				environmentLabel: productionValue,
			}
			err = r.Update(context.Background(), updatedClusterProfile)
			require.NoError(t, err)

			_, err = r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.NotContains(t, secret.Labels, teamLabel)
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, secret.Labels[clusterProfileNameKey])
		})

		t.Run("should update the secret when the ClusterProfile is updated", func(t *testing.T) {
			providersFile := writeProvidersFile(t)

			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
				Status: clusterinventory.ClusterProfileStatus{
					AccessProviders: []clusterinventory.AccessProvider{
						{
							Name: testProviderName,
							Cluster: clientcmdv1.Cluster{
								Server: testServer,
							},
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			// Update the ClusterProfile
			updatedClusterProfile := &clusterinventory.ClusterProfile{}
			err = r.Get(context.Background(), req.NamespacedName, updatedClusterProfile)
			require.NoError(t, err)
			updatedClusterProfile.Status.AccessProviders[0].Cluster.Server = "https://updated-cluster.example.com"
			err = r.Update(context.Background(), updatedClusterProfile)
			require.NoError(t, err)

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			require.NoError(t, err)
			assert.Equal(t, "https://updated-cluster.example.com", string(secret.Data[secretDataServerKey]))
		})

		t.Run("should not return an error if the ClusterProfile is not found", func(t *testing.T) {
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			res, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			assert.Equal(t, reconcile.Result{}, res)
		})

		t.Run("should do nothing while the ClusterProfile is being deleted", func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.DeletionTimestamp = &now
			clusterProfile.Finalizers = []string{"example.com/unrelated"}
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			// The secret's owner reference leaves the cleanup to the garbage collector.
			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			assert.True(t, apierrors.IsNotFound(err))
		})

		t.Run("should reject persisted Secrets without current provenance", func(t *testing.T) {
			testCases := []struct {
				name                  string
				secretLabels          map[string]string
				secretOwnerReferences []metav1.OwnerReference
			}{
				{
					name: "ownerless manual Secret",
					secretLabels: map[string]string{
						managedByLabel: "human",
					},
				},
				{
					name: "generated-looking labels without an owner",
					secretLabels: map[string]string{
						clusterProfileNameKey:     testClusterName,
						common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
					},
				},
				{
					name: "matching owner identity with a stale UID",
					secretLabels: map[string]string{
						clusterProfileNameKey:     testClusterName,
						common.LabelKeySecretType: common.LabelValueSecretTypeCluster,
					},
					secretOwnerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference(testClusterName, "stale-cluster-profile-uid"),
					},
				},
				{
					name: "another ClusterProfile as the controller",
					secretLabels: map[string]string{
						managedByLabel: "foreign-controller",
					},
					secretOwnerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference("another-cluster", "another-uid"),
					},
				},
			}

			for _, testCase := range testCases {
				t.Run(testCase.name, func(t *testing.T) {
					clusterProfile := newBuiltinProviderClusterProfile(nil)
					clusterProfile.Namespace = argocdNamespace
					clusterProfile.UID = testClusterProfileUID
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:            testSecretName,
							Namespace:       argocdNamespace,
							UID:             "persisted-secret-uid",
							ResourceVersion: "42",
							Labels:          maps.Clone(testCase.secretLabels),
							OwnerReferences: slices.Clone(testCase.secretOwnerReferences),
						},
						Data: map[string][]byte{
							secretDataNameKey:   []byte("manual-cluster"),
							secretDataServerKey: []byte("https://manual.example.com"),
							secretDataConfigKey: []byte(`{"manual":true}`),
						},
					}
					before := secret.DeepCopy()
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}
					req := reconcile.Request{
						NamespacedName: types.NamespacedName{
							Name:      testClusterName,
							Namespace: argocdNamespace,
						},
					}

					_, err := r.Reconcile(context.Background(), req)

					require.ErrorContains(t, err, "refusing to mutate Secret")
					var preservedSecret corev1.Secret
					require.NoError(
						t,
						r.Get(
							context.Background(),
							types.NamespacedName{Name: testSecretName, Namespace: argocdNamespace},
							&preservedSecret,
						),
					)
					assertSecretPreserved(t, before, &preservedSecret)
				})
			}
		})

		t.Run("should repair a Secret controlled by the current ClusterProfile UID", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.Namespace = argocdNamespace
			clusterProfile.UID = testClusterProfileUID
			secret := newControlledSecret(clusterProfile, "current-secret-uid")
			secret.Labels = map[string]string{"app.example.com/stale": trueValue}
			seedStaleClusterSecretData(secret)
			countingClient := &updateCountingClient{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
			}
			r := &ClusterProfileReconciler{
				Client: countingClient,
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: argocdNamespace},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var repairedSecret corev1.Secret
			require.NoError(
				t,
				r.Get(
					context.Background(),
					types.NamespacedName{Name: testSecretName, Namespace: argocdNamespace},
					&repairedSecret,
				),
			)
			assert.Equal(t, secret.UID, repairedSecret.UID)
			assert.Equal(t, common.LabelValueSecretTypeCluster, repairedSecret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testClusterName, repairedSecret.Labels[clusterProfileNameKey])
			assert.NotContains(t, repairedSecret.Labels, "app.example.com/stale")
			config := requireExactTestClusterSecretData(t, &repairedSecret)
			require.NotNil(t, config.ExecProviderConfig)
			assert.Equal(t, "argocd-k8s-auth", config.ExecProviderConfig.Command)
			assert.Equal(t, []string{"gcp"}, config.ExecProviderConfig.Args)
			require.Len(t, repairedSecret.OwnerReferences, 1)
			assert.Equal(t, clusterProfile.UID, repairedSecret.OwnerReferences[0].UID)
			repairedResourceVersion := repairedSecret.ResourceVersion
			require.Equal(t, 1, countingClient.updates)

			_, err = r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			assert.Equal(t, 1, countingClient.updates)
			var stableSecret corev1.Secret
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &stableSecret))
			assert.Equal(t, repairedResourceVersion, stableSecret.ResourceVersion)
			requireExactTestClusterSecretData(t, &stableSecret)
		})

		t.Run("should replace stale provider data on a Secret owned by the same ClusterProfile UID", func(t *testing.T) {
			providersFile := writeProvidersFile(t)
			clusterProfile := newCustomProviderClusterProfile(nil)
			secret := newControlledSecret(clusterProfile, "custom-secret-uid")
			seedStaleClusterSecretData(secret)
			countingClient := &updateCountingClient{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
			}
			r := &ClusterProfileReconciler{
				Client:                     countingClient,
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())
			req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(clusterProfile)}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			require.Equal(t, 1, countingClient.updates)
			var repairedSecret corev1.Secret
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &repairedSecret))
			config := requireExactTestClusterSecretData(t, &repairedSecret)
			require.NotNil(t, config.ExecProviderConfig)
			assert.Equal(t, testProviderCommand, config.ExecProviderConfig.Command)
			repairedResourceVersion := repairedSecret.ResourceVersion

			_, err = r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			assert.Equal(t, 1, countingClient.updates)
			var stableSecret corev1.Secret
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &stableSecret))
			assert.Equal(t, repairedResourceVersion, stableSecret.ResourceVersion)
			requireExactTestClusterSecretData(t, &stableSecret)
		})

		t.Run("should succeed idempotently when no access provider or Secret exists", func(t *testing.T) {
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
					UID:       testClusterProfileUID,
				},
			}
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace},
			}

			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)

			var secret corev1.Secret
			err = r.Get(
				context.Background(),
				types.NamespacedName{Name: testSecretName, Namespace: testNamespace},
				&secret,
			)
			assert.True(t, apierrors.IsNotFound(err))
		})

		t.Run("should recreate a revoked Secret owned by the same ClusterProfile UID after recovery", func(t *testing.T) {
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
					UID:       testClusterProfileUID,
				},
			}
			secret := newControlledSecret(clusterProfile, "revoked-secret-uid")
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace},
			}

			_, err := r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			var deletedSecret corev1.Secret
			err = r.Get(context.Background(), client.ObjectKeyFromObject(secret), &deletedSecret)
			assert.True(t, apierrors.IsNotFound(err))

			var recoveredProfile clusterinventory.ClusterProfile
			require.NoError(t, r.Get(context.Background(), req.NamespacedName, &recoveredProfile))
			recoveredProfile.Status.AccessProviders = []clusterinventory.AccessProvider{
				{
					Name: builtinGCPProviderName,
					Cluster: clientcmdv1.Cluster{
						Server: testServer,
					},
				},
			}
			require.NoError(t, r.Update(context.Background(), &recoveredProfile))

			_, err = r.Reconcile(context.Background(), req)
			require.NoError(t, err)
			var recreatedSecret corev1.Secret
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &recreatedSecret))
			require.Len(t, recreatedSecret.OwnerReferences, 1)
			assert.Equal(t, clusterProfile.UID, recreatedSecret.OwnerReferences[0].UID)
			assert.Equal(t, testServer, string(recreatedSecret.Data[secretDataServerKey]))
		})

		t.Run("should preserve Secrets without the current ClusterProfile owner after access is revoked", func(t *testing.T) {
			controller := true
			testCases := []struct {
				name            string
				ownerReferences []metav1.OwnerReference
			}{
				{
					name: "ownerless Secret",
				},
				{
					name: "foreign-controlled Secret",
					ownerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Name:       "foreign-controller",
							UID:        "foreign-controller-uid",
							Controller: &controller,
						},
					},
				},
				{
					name: "Secret controlled by a previous ClusterProfile UID",
					ownerReferences: []metav1.OwnerReference{
						clusterProfileOwnerReference(testClusterName, "previous-cluster-profile-uid"),
					},
				},
			}

			for _, testCase := range testCases {
				t.Run(testCase.name, func(t *testing.T) {
					clusterProfile := &clusterinventory.ClusterProfile{
						ObjectMeta: metav1.ObjectMeta{
							Name:      testClusterName,
							Namespace: testNamespace,
							UID:       testClusterProfileUID,
						},
					}
					secret := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{
							Name:            testSecretName,
							Namespace:       testNamespace,
							UID:             "preserved-secret-uid",
							ResourceVersion: "42",
							Labels: map[string]string{
								managedByLabel: "external",
							},
							OwnerReferences: slices.Clone(testCase.ownerReferences),
						},
						Data: map[string][]byte{
							secretDataNameKey:   []byte("external-cluster"),
							secretDataServerKey: []byte("https://external.example.com"),
							secretDataConfigKey: []byte(`{"external":true}`),
						},
					}
					before := secret.DeepCopy()
					r := &ClusterProfileReconciler{
						Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
						Log:    logr.Discard(),
						Scheme: scheme,
					}
					req := reconcile.Request{
						NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace},
					}

					_, err := r.Reconcile(context.Background(), req)
					require.NoError(t, err)

					var preservedSecret corev1.Secret
					require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &preservedSecret))
					assertSecretPreserved(t, before, &preservedSecret)
				})
			}
		})

		t.Run("should use delete preconditions and retry when revoked Secret deletion conflicts", func(t *testing.T) {
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
					UID:       testClusterProfileUID,
				},
			}
			secret := newControlledSecret(clusterProfile, "revoked-secret-uid")
			baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build()
			conflictClient := &deleteConflictClient{Client: baseClient}
			r := &ClusterProfileReconciler{
				Client: conflictClient,
				Log:    logr.Discard(),
				Scheme: scheme,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.Error(t, err)
			assert.True(t, apierrors.IsConflict(err))
			assertDeletePreconditions(t, conflictClient, secret)

			var preservedSecret corev1.Secret
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), &preservedSecret))
			assert.Equal(t, secret.UID, preservedSecret.UID)
		})

		t.Run("should continue using a deprecated credential provider when access providers are empty", func(t *testing.T) {
			providersFile := writeProvidersFile(t)
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
					UID:       testClusterProfileUID,
				},
				Status: clusterinventory.ClusterProfileStatus{
					CredentialProviders: []clusterinventory.CredentialProvider{
						{
							Name: testProviderName,
							Cluster: clientcmdv1.Cluster{
								Server: testServer,
							},
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client:                     fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:                        logr.Discard(),
				Scheme:                     scheme,
				ClusterProfileProviderFile: providersFile,
			}
			require.NoError(t, r.loadClusterProfileProviderFile())
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: testNamespace},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var secret corev1.Secret
			require.NoError(
				t,
				r.Get(
					context.Background(),
					types.NamespacedName{Name: testSecretName, Namespace: testNamespace},
					&secret,
				),
			)
			assert.Equal(t, testServer, string(secret.Data[secretDataServerKey]))
			require.Len(t, secret.OwnerReferences, 1)
			assert.Equal(t, clusterProfile.UID, secret.OwnerReferences[0].UID)
		})

		t.Run("should create a secret per namespace for same-named ClusterProfiles", func(t *testing.T) {
			profileA := newBuiltinProviderClusterProfile(nil)
			profileA.Namespace = teamANamespace
			profileA.UID = "uid-team-a"
			profileB := newBuiltinProviderClusterProfile(nil)
			profileB.Namespace = teamBNamespace
			profileB.UID = "uid-team-b"
			profileB.Status.AccessProviders[0].Cluster.Server = "https://team-b.example.com"
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(profileA, profileB).Build(),
				Log:    logr.Discard(),
				Scheme: scheme,
			}

			for _, namespace := range []string{teamANamespace, teamBNamespace} {
				_, err := r.Reconcile(context.Background(), reconcile.Request{
					NamespacedName: types.NamespacedName{Name: testClusterName, Namespace: namespace},
				})
				require.NoError(t, err)
			}

			var secretA corev1.Secret
			require.NoError(
				t,
				r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: teamANamespace}, &secretA),
			)
			assert.Equal(t, testClusterName, secretA.Labels[clusterProfileNameKey])
			assert.Equal(t, testServer, string(secretA.Data[secretDataServerKey]))
			require.Len(t, secretA.OwnerReferences, 1)
			assert.Equal(t, types.UID("uid-team-a"), secretA.OwnerReferences[0].UID)

			var secretB corev1.Secret
			require.NoError(
				t,
				r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: teamBNamespace}, &secretB),
			)
			assert.Equal(t, testClusterName, secretB.Labels[clusterProfileNameKey])
			assert.Equal(t, "https://team-b.example.com", string(secretB.Data[secretDataServerKey]))
			require.Len(t, secretB.OwnerReferences, 1)
			assert.Equal(t, types.UID("uid-team-b"), secretB.OwnerReferences[0].UID)
		})

	})
}

func TestLoadClusterProfileProviderFile(t *testing.T) {
	t.Run("should not return an error if the provider file is not specified", func(t *testing.T) {
		r := &ClusterProfileReconciler{
			Log: logr.Discard(),
		}
		err := r.loadClusterProfileProviderFile()
		assert.NoError(t, err)
	})

	t.Run("should return an error if the provider file does not exist", func(t *testing.T) {
		r := &ClusterProfileReconciler{
			Log:                        logr.Discard(),
			ClusterProfileProviderFile: "non-existent-file",
		}
		err := r.loadClusterProfileProviderFile()
		assert.Error(t, err)
	})
}

func TestNewCommandClusterProfileProviderFileFlag(t *testing.T) {
	t.Setenv("ARGOCD_CLUSTERPROFILE_CONTROLLER_CLUSTERPROFILE_PROVIDER_FILE", "/tmp/access.json")

	command := NewCommand()
	providerFileFlag := command.Flags().Lookup("clusterprofile-provider-file")

	require.NotNil(t, providerFileFlag)
	assert.Equal(t, "/tmp/access.json", providerFileFlag.DefValue)
	assert.Nil(t, command.Flags().Lookup("cluster-profile-providers-file"))
}

func TestNewCommandClusterProfileNamespacesAllNamespacesSentinel(t *testing.T) {
	t.Setenv("ARGOCD_CLUSTERPROFILE_CONTROLLER_NAMESPACES", "*")

	command := NewCommand()

	namespacesFlag := command.Flags().Lookup("cluster-profile-namespaces")
	require.NotNil(t, namespacesFlag)
	assert.Equal(t, "[*]", namespacesFlag.DefValue)

	// The dedicated boolean flag has been removed in favour of the "*" sentinel.
	assert.Nil(t, command.Flags().Lookup("cluster-profile-all-namespaces"))
}

func TestBuildCacheOptions(t *testing.T) {
	t.Run("defaults ClusterProfiles and Secrets to the controller namespace", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, nil)

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{argocdNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("defaults ClusterProfiles to the controller namespace when namespace entries are blank", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"", " ", "\t"})

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{argocdNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("limits ClusterProfiles and Secrets to the same explicit namespaces", func(t *testing.T) {
		options := buildCacheOptions(
			argocdNamespace,
			[]string{teamANamespace, " team-b ", teamANamespace, ""},
		)

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{teamANamespace, teamBNamespace})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{teamANamespace, teamBNamespace})
	})

	t.Run("watches ClusterProfiles and Secrets in all namespaces when the wildcard is requested", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"*"})

		assertAllNamespacesCache(t, options, &clusterinventory.ClusterProfile{})
		assertAllNamespacesCache(t, options, &corev1.Secret{})
	})

	t.Run("the wildcard takes precedence over explicit namespaces", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{teamANamespace, "*"})

		assertAllNamespacesCache(t, options, &clusterinventory.ClusterProfile{})
		assertAllNamespacesCache(t, options, &corev1.Secret{})
	})
}

func assertCacheNamespaces(
	t *testing.T,
	options cache.Options,
	object client.Object,
	expectedNamespaces []string,
) {
	t.Helper()

	namespaces := cacheNamespacesFor(t, options, object)
	require.NotNil(t, namespaces)
	assert.ElementsMatch(t, expectedNamespaces, slices.Collect(maps.Keys(namespaces)))
}

// assertAllNamespacesCache asserts the object is cached cluster-wide: an empty (but non-nil)
// namespace map defers to the default cluster-wide cache.
func assertAllNamespacesCache(t *testing.T, options cache.Options, object client.Object) {
	t.Helper()

	namespaces := cacheNamespacesFor(t, options, object)
	require.NotNil(t, namespaces)
	assert.Empty(t, namespaces)
}

func cacheNamespacesFor(
	t *testing.T,
	options cache.Options,
	object client.Object,
) map[string]cache.Config {
	t.Helper()

	objectType := reflect.TypeOf(object)
	for cachedObject, byObject := range options.ByObject {
		if reflect.TypeOf(cachedObject) == objectType {
			return byObject.Namespaces
		}
	}
	t.Fatalf("cache options do not include object type %T", object)
	return nil
}
