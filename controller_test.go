package main

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/argoproj/argo-cd/v3/common"
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	clusterinventory "sigs.k8s.io/cluster-inventory-api/apis/v1alpha1"
	"sigs.k8s.io/cluster-inventory-api/pkg/access"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testClusterName      = "test-cluster"
	testNamespace        = "default"
	testServer           = "https://test-cluster.example.com"
	testSecretName       = "cluster-test-cluster"
	testOriginLabelValue = "default-test-cluster"
	testProviderName     = "secretreader"
	testProviderCommand  = "/plugins/secretreader/bin/secretreader-plugin"
	argocdNamespace      = "argocd"
	environmentLabel     = "environment"
	productionValue      = "production"
	teamANamespace       = "team-a"
	teamBNamespace       = "team-b"
)

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
					Name: "argo-cd-builtin-gcp",
					Cluster: clientcmdv1.Cluster{
						Server: testServer,
					},
				},
			},
		},
	}
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
			controller := true
			blockOwnerDeletion := false
			assert.Equal(t, []metav1.OwnerReference{{
				APIVersion:         "multicluster.x-k8s.io/v1alpha1",
				Kind:               "ClusterProfile",
				Name:               testClusterName,
				Controller:         &controller,
				BlockOwnerDeletion: &blockOwnerDeletion,
			}}, secret.OwnerReferences)
			assert.Equal(t, "cluster", secret.Labels["argocd.argoproj.io/secret-type"])
			assert.Equal(t, testOriginLabelValue, secret.Labels["argocd.argoproj.io/cluster-profile-origin"])
			assert.Equal(t, testClusterName, secret.StringData[secretDataNameKey])
			assert.Equal(t, testServer, secret.StringData[secretDataServerKey])

			var configMap map[string]any
			require.NoError(t, json.Unmarshal([]byte(secret.StringData[secretDataConfigKey]), &configMap))
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
			require.NoError(t, json.Unmarshal([]byte(secret.StringData[secretDataConfigKey]), &configMap))
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
							Name: "argo-cd-builtin-gcp",
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
				"team":           "platform",
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
			assert.Equal(t, "platform", secret.Labels["team"])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testOriginLabelValue, secret.Labels[clusterProfileOriginLabel])
		})

		t.Run("should protect controller-owned secret labels from ClusterProfile labels", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				common.LabelKeySecretType: "not-cluster",
				clusterProfileOriginLabel: "other-origin",
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
			assert.Equal(t, testOriginLabelValue, secret.Labels[clusterProfileOriginLabel])
		})

		t.Run("should update secret labels when ClusterProfile labels change", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: "staging",
				"team":           "platform",
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
			assert.NotContains(t, secret.Labels, "team")
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assert.Equal(t, testOriginLabelValue, secret.Labels[clusterProfileOriginLabel])
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
			assert.Equal(t, "https://updated-cluster.example.com", secret.StringData[secretDataServerKey])
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

			require.NoError(t, err)
			var secret corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &secret)
			assert.True(t, apierrors.IsNotFound(err))
		})

		t.Run("should return an error if access providers are empty", func(t *testing.T) {
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
				Status: clusterinventory.ClusterProfileStatus{
					AccessProviders: []clusterinventory.AccessProvider{},
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
			err := r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: teamANamespace}, &secretA)
			require.NoError(t, err)
			assert.Equal(t, "team-a-test-cluster", secretA.Labels[clusterProfileOriginLabel])
			assert.Equal(t, testServer, secretA.StringData[secretDataServerKey])
			require.Len(t, secretA.OwnerReferences, 1)
			assert.Equal(t, types.UID("uid-team-a"), secretA.OwnerReferences[0].UID)

			var secretB corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: teamBNamespace}, &secretB)
			require.NoError(t, err)
			assert.Equal(t, "team-b-test-cluster", secretB.Labels[clusterProfileOriginLabel])
			assert.Equal(t, "https://team-b.example.com", secretB.StringData[secretDataServerKey])
			require.Len(t, secretB.OwnerReferences, 1)
			assert.Equal(t, types.UID("uid-team-b"), secretB.OwnerReferences[0].UID)
		})

		t.Run("should not adopt a secret controlled by another owner", func(t *testing.T) {
			controller := true
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      testSecretName,
					Namespace: testNamespace,
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "multicluster.x-k8s.io/v1alpha1",
							Kind:       "ClusterProfile",
							Name:       "another-cluster",
							UID:        "another-uid",
							Controller: &controller,
						},
					},
				},
			}
			r := &ClusterProfileReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
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
			var unchanged corev1.Secret
			err = r.Get(context.Background(), types.NamespacedName{Name: testSecretName, Namespace: testNamespace}, &unchanged)
			require.NoError(t, err)
			require.Len(t, unchanged.OwnerReferences, 1)
			assert.Equal(t, types.UID("another-uid"), unchanged.OwnerReferences[0].UID)
			assert.Empty(t, unchanged.Labels)
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

	t.Run("watches ClusterProfiles and Secrets in the requested namespaces", func(t *testing.T) {
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
