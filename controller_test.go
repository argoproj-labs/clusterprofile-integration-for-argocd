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
	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/validate/content"
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
	testClusterName     = "test-cluster"
	testNamespace       = "default"
	teamNamespace       = "team"
	testServer          = "https://test-cluster.example.com"
	testProviderName    = "secretreader"
	testProviderCommand = "/plugins/secretreader/bin/secretreader-plugin"
	argocdNamespace     = "argocd"
	environmentLabel    = "environment"
	teamLabel           = "team"
	productionValue     = "production"
)

type failingDeleteClient struct {
	client.Client
}

func (c *failingDeleteClient) Delete(_ context.Context, _ client.Object, _ ...client.DeleteOption) error {
	return errors.New("unable to delete secret")
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
					Name: "argo-cd-builtin-gcp",
					Cluster: clientcmdv1.Cluster{
						Server: testServer,
					},
				},
			},
		},
	}
}

func getGeneratedSecret(
	t *testing.T,
	r *ClusterProfileReconciler,
	clusterProfile *clusterinventory.ClusterProfile,
) corev1.Secret {
	t.Helper()

	var secret corev1.Secret
	err := r.Get(
		context.Background(),
		types.NamespacedName{Name: clusterSecretName(clusterProfile), Namespace: argocdNamespace},
		&secret,
	)
	require.NoError(t, err)
	return secret
}

func assertSecretIdentity(
	t *testing.T,
	secret corev1.Secret,
	clusterProfile *clusterinventory.ClusterProfile,
) {
	t.Helper()

	assert.Equal(t, clusterProfile.Namespace, secret.Labels[clusterProfileNamespaceLabel])
	assert.Equal(t, clusterProfile.Name, secret.Labels[clusterProfileNameLabel])
	assert.Equal(t, clusterProfile.Namespace+"/"+clusterProfile.Name, secret.StringData[secretDataNameKey])
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
				Namespace:                  argocdNamespace,
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
			secret := getGeneratedSecret(t, r, clusterProfile)
			assert.Equal(t, "cluster-test-cluster-a6fbd023908c", secret.Name)
			assert.Equal(t, argocdNamespace, secret.Namespace)
			assert.Equal(t, "cluster", secret.Labels["argocd.argoproj.io/secret-type"])
			assertSecretIdentity(t, secret, clusterProfile)
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

		t.Run("should create distinct secrets for same-named ClusterProfiles in different namespaces", func(t *testing.T) {
			const otherNamespace = "other-namespace"
			const otherServer = "https://other-cluster.example.com"
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			otherClusterProfile := newBuiltinProviderClusterProfile(nil)
			otherClusterProfile.Namespace = otherNamespace
			otherClusterProfile.Status.AccessProviders[0].Cluster.Server = otherServer
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, otherClusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(clusterProfile),
			})
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(otherClusterProfile),
			})
			require.NoError(t, err)

			secret := getGeneratedSecret(t, r, clusterProfile)
			assertSecretIdentity(t, secret, clusterProfile)
			assert.Equal(t, testServer, secret.StringData[secretDataServerKey])

			otherSecret := getGeneratedSecret(t, r, otherClusterProfile)
			assertSecretIdentity(t, otherSecret, otherClusterProfile)
			assert.Equal(t, otherServer, otherSecret.StringData[secretDataServerKey])
		})

		t.Run("should create collision-free secrets for hyphen-ambiguous ClusterProfiles", func(t *testing.T) {
			clusterProfileA := newBuiltinProviderClusterProfile(nil)
			clusterProfileA.Namespace = teamNamespace
			clusterProfileA.Name = "a-prod"
			clusterProfileB := newBuiltinProviderClusterProfile(nil)
			clusterProfileB.Namespace = "team-a"
			clusterProfileB.Name = "prod"
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfileA, clusterProfileB).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(clusterProfileA),
			})
			require.NoError(t, err)
			_, err = r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(clusterProfileB),
			})
			require.NoError(t, err)

			secretA := getGeneratedSecret(t, r, clusterProfileA)
			secretB := getGeneratedSecret(t, r, clusterProfileB)
			assert.NotEqual(t, secretA.Name, secretB.Name)
			assertSecretIdentity(t, secretA, clusterProfileA)
			assertSecretIdentity(t, secretB, clusterProfileB)
		})

		t.Run("should keep generated Secret metadata within Kubernetes limits", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(nil)
			clusterProfile.Name = strings.Repeat("a", content.DNS1123SubdomainMaxLength)
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}

			_, err := r.Reconcile(context.Background(), reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(clusterProfile),
			})

			require.NoError(t, err)
			secret := getGeneratedSecret(t, r, clusterProfile)
			assert.Empty(t, content.IsDNS1123Subdomain(secret.Name))
			assert.Equal(t, strings.Repeat("a", 50)+"-4a6f19b4d248", secret.Labels[clusterProfileNameLabel])
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
				Namespace:                  argocdNamespace,
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

			secret := getGeneratedSecret(t, r, clusterProfile)
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
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
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
			err = r.Get(
				context.Background(),
				types.NamespacedName{Name: clusterSecretName(clusterProfile), Namespace: argocdNamespace},
				&secret,
			)
			assert.True(t, apierrors.IsNotFound(err))
		})

		t.Run("should propagate ClusterProfile labels to the generated secret", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: productionValue,
				teamLabel:        "platform",
			})
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			secret := getGeneratedSecret(t, r, clusterProfile)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, "platform", secret.Labels[teamLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assertSecretIdentity(t, secret, clusterProfile)
		})

		t.Run("should protect controller-owned secret labels from ClusterProfile labels", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				common.LabelKeySecretType:    "not-cluster",
				clusterProfileNamespaceLabel: "other-namespace",
				clusterProfileNameLabel:      "other-name",
				environmentLabel:             productionValue,
			})
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			secret := getGeneratedSecret(t, r, clusterProfile)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assertSecretIdentity(t, secret, clusterProfile)
		})

		t.Run("should update secret labels when ClusterProfile labels change", func(t *testing.T) {
			clusterProfile := newBuiltinProviderClusterProfile(map[string]string{
				environmentLabel: "staging",
				teamLabel:        "platform",
			})
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
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
			secret := getGeneratedSecret(t, r, updatedClusterProfile)
			assert.Equal(t, productionValue, secret.Labels[environmentLabel])
			assert.NotContains(t, secret.Labels, teamLabel)
			assert.Equal(t, common.LabelValueSecretTypeCluster, secret.Labels[common.LabelKeySecretType])
			assertSecretIdentity(t, secret, updatedClusterProfile)
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
				Namespace:                  argocdNamespace,
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

			secret := getGeneratedSecret(t, r, clusterProfile)
			assert.Equal(t, "https://updated-cluster.example.com", secret.StringData[secretDataServerKey])
		})

		t.Run("should not return an error if the ClusterProfile is not found", func(t *testing.T) {
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
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

		t.Run("should add a finalizer if it is not present", func(t *testing.T) {
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
				Namespace:                  argocdNamespace,
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
			var updatedClusterProfile clusterinventory.ClusterProfile
			err = r.Get(context.Background(), req.NamespacedName, &updatedClusterProfile)
			require.NoError(t, err)
			assert.Contains(t, updatedClusterProfile.Finalizers, clusterProfileFinalizer)
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
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
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

		t.Run("should remove the finalizer if the secret does not exist on prune", func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:              testClusterName,
					Namespace:         testNamespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{clusterProfileFinalizer},
				},
			}
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			// The reconcile should not return an error, and the garbage collector should delete the resource.
			require.NoError(t, err)
		})

		t.Run("should delete the secret when the ClusterProfile is deleted", func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:              testClusterName,
					Namespace:         testNamespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{clusterProfileFinalizer},
				},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterSecretName(clusterProfile),
					Namespace: argocdNamespace,
				},
			}
			r := &ClusterProfileReconciler{
				Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			require.NoError(t, err)
			var deletedSecret corev1.Secret
			err = r.Get(context.Background(), client.ObjectKeyFromObject(secret), &deletedSecret)
			assert.Error(t, err)
		})

		t.Run("should not remove the finalizer if secret deletion fails", func(t *testing.T) {
			now := metav1.NewTime(time.Now())
			clusterProfile := &clusterinventory.ClusterProfile{
				ObjectMeta: metav1.ObjectMeta{
					Name:              testClusterName,
					Namespace:         testNamespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{clusterProfileFinalizer},
				},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      clusterSecretName(clusterProfile),
					Namespace: argocdNamespace,
				},
			}
			r := &ClusterProfileReconciler{
				Client: &failingDeleteClient{
					Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(clusterProfile, secret).Build(),
				},
				Log:       logr.Discard(),
				Scheme:    scheme,
				Namespace: argocdNamespace,
			}
			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testClusterName,
					Namespace: testNamespace,
				},
			}

			_, err := r.Reconcile(context.Background(), req)

			// The reconcile should return an error because the secret deletion fails.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unable to delete secret")
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

	t.Run("watches only the requested ClusterProfile namespaces", func(t *testing.T) {
		const teamANamespace = "team-a"

		options := buildCacheOptions(
			argocdNamespace,
			[]string{teamANamespace, " team-b ", teamANamespace, ""},
		)

		assertCacheNamespaces(t, options, &clusterinventory.ClusterProfile{}, []string{teamANamespace, "team-b"})
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("watches ClusterProfiles in all namespaces when the wildcard is requested", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"*"})

		clusterProfileNamespaces := cacheNamespacesFor(t, options, &clusterinventory.ClusterProfile{})
		require.NotNil(t, clusterProfileNamespaces)
		assert.Empty(t, clusterProfileNamespaces)
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
	})

	t.Run("the wildcard takes precedence over explicit namespaces", func(t *testing.T) {
		options := buildCacheOptions(argocdNamespace, []string{"team-a", "*"})

		clusterProfileNamespaces := cacheNamespacesFor(t, options, &clusterinventory.ClusterProfile{})
		require.NotNil(t, clusterProfileNamespaces)
		assert.Empty(t, clusterProfileNamespaces)
		assertCacheNamespaces(t, options, &corev1.Secret{}, []string{argocdNamespace})
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
