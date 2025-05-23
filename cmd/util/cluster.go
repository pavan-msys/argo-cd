package util

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapiv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	"sigs.k8s.io/yaml"

	argoappv1 "github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/v3/util/errors"
)

type ClusterEndpoint string

const (
	KubeConfigEndpoint   ClusterEndpoint = "kubeconfig"
	KubePublicEndpoint   ClusterEndpoint = "kube-public"
	KubeInternalEndpoint ClusterEndpoint = "internal"
)

func PrintKubeContexts(ca clientcmd.ConfigAccess) {
	config, err := ca.GetStartingConfig()
	errors.CheckError(err)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer func() { _ = w.Flush() }()
	columnNames := []string{"CURRENT", "NAME", "CLUSTER", "SERVER"}
	_, err = fmt.Fprintf(w, "%s\n", strings.Join(columnNames, "\t"))
	errors.CheckError(err)

	// sort names so output is deterministic
	contextNames := make([]string, 0)
	for name := range config.Contexts {
		contextNames = append(contextNames, name)
	}
	sort.Strings(contextNames)

	if config.Clusters == nil {
		return
	}

	for _, name := range contextNames {
		// ignore malformed kube config entries
		context := config.Contexts[name]
		if context == nil {
			continue
		}
		cluster := config.Clusters[context.Cluster]
		if cluster == nil {
			continue
		}
		prefix := " "
		if config.CurrentContext == name {
			prefix = "*"
		}
		_, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", prefix, name, context.Cluster, cluster.Server)
		errors.CheckError(err)
	}
}

func NewCluster(name string, namespaces []string, clusterResources bool, conf *rest.Config, managerBearerToken string, awsAuthConf *argoappv1.AWSAuthConfig, execProviderConf *argoappv1.ExecProviderConfig, labels, annotations map[string]string) *argoappv1.Cluster {
	tlsClientConfig := argoappv1.TLSClientConfig{
		Insecure:   conf.Insecure,
		ServerName: conf.ServerName,
		CAData:     conf.CAData,
		CertData:   conf.CertData,
		KeyData:    conf.KeyData,
	}
	if len(conf.CAData) == 0 && conf.CAFile != "" {
		data, err := os.ReadFile(conf.CAFile)
		errors.CheckError(err)
		tlsClientConfig.CAData = data
	}
	if len(conf.CertData) == 0 && conf.CertFile != "" {
		data, err := os.ReadFile(conf.CertFile)
		errors.CheckError(err)
		tlsClientConfig.CertData = data
	}
	if len(conf.KeyData) == 0 && conf.KeyFile != "" {
		data, err := os.ReadFile(conf.KeyFile)
		errors.CheckError(err)
		tlsClientConfig.KeyData = data
	}

	clst := argoappv1.Cluster{
		Server:           conf.Host,
		Name:             name,
		Namespaces:       namespaces,
		ClusterResources: clusterResources,
		Config: argoappv1.ClusterConfig{
			TLSClientConfig:    tlsClientConfig,
			AWSAuthConfig:      awsAuthConf,
			ExecProviderConfig: execProviderConf,
			DisableCompression: conf.DisableCompression,
		},
		Labels:      labels,
		Annotations: annotations,
	}
	// it's a tradeoff to get proxy url from rest config
	// more detail: https://github.com/kubernetes/kubernetes/pull/81443
	if conf.Proxy != nil {
		if url, err := conf.Proxy(nil); err == nil {
			clst.Config.ProxyUrl = url.String()
		}
	}
	// Bearer token will preferentially be used for auth if present,
	// Even in presence of key/cert credentials
	// So set bearer token only if the key/cert data is absent
	if len(tlsClientConfig.CertData) == 0 || len(tlsClientConfig.KeyData) == 0 {
		clst.Config.BearerToken = managerBearerToken
	}

	return &clst
}

// GetKubePublicEndpoint returns the kubernetes apiserver endpoint and certificate authority data as published
// in the kube-public.
func GetKubePublicEndpoint(client kubernetes.Interface) (string, []byte, error) {
	clusterInfo, err := client.CoreV1().ConfigMaps("kube-public").Get(context.TODO(), "cluster-info", metav1.GetOptions{})
	if err != nil {
		return "", nil, err
	}
	kubeconfig, ok := clusterInfo.Data["kubeconfig"]
	if !ok {
		return "", nil, stderrors.New("cluster-info does not contain a public kubeconfig")
	}
	// Parse Kubeconfig and get server address
	config := &clientcmdapiv1.Config{}
	err = yaml.Unmarshal([]byte(kubeconfig), config)
	if err != nil {
		return "", nil, fmt.Errorf("failed to parse cluster-info kubeconfig: %w", err)
	}
	if len(config.Clusters) == 0 {
		return "", nil, stderrors.New("cluster-info kubeconfig does not have any clusters")
	}

	endpoint := config.Clusters[0].Cluster.Server
	certificateAuthorityData := config.Clusters[0].Cluster.CertificateAuthorityData
	return endpoint, certificateAuthorityData, nil
}

type ClusterOptions struct {
	InCluster               bool
	Upsert                  bool
	ServiceAccount          string
	AwsRoleArn              string
	AwsProfile              string
	AwsClusterName          string
	SystemNamespace         string
	Namespaces              []string
	ClusterResources        bool
	Name                    string
	Project                 string
	Shard                   int64
	ExecProviderCommand     string
	ExecProviderArgs        []string
	ExecProviderEnv         map[string]string
	ExecProviderAPIVersion  string
	ExecProviderInstallHint string
	ClusterEndpoint         string
	DisableCompression      bool
	ProxyUrl                string //nolint:revive //FIXME(var-naming)
}

// InClusterEndpoint returns true if ArgoCD should reference the in-cluster
// endpoint when registering the target cluster.
func (o ClusterOptions) InClusterEndpoint() bool {
	return o.InCluster || o.ClusterEndpoint == string(KubeInternalEndpoint)
}

func AddClusterFlags(command *cobra.Command, opts *ClusterOptions) {
	command.Flags().BoolVar(&opts.InCluster, "in-cluster", false, "Indicates Argo CD resides inside this cluster and should connect using the internal k8s hostname (kubernetes.default.svc)")
	command.Flags().StringVar(&opts.AwsClusterName, "aws-cluster-name", "", "AWS Cluster name if set then aws cli eks token command will be used to access cluster")
	command.Flags().StringVar(&opts.AwsRoleArn, "aws-role-arn", "", "Optional AWS role arn. If set then AWS IAM Authenticator assumes a role to perform cluster operations instead of the default AWS credential provider chain.")
	command.Flags().StringVar(&opts.AwsProfile, "aws-profile", "", "Optional AWS profile. If set then AWS IAM Authenticator uses this profile to perform cluster operations instead of the default AWS credential provider chain.")
	command.Flags().StringArrayVar(&opts.Namespaces, "namespace", nil, "List of namespaces which are allowed to manage")
	command.Flags().BoolVar(&opts.ClusterResources, "cluster-resources", false, "Indicates if cluster level resources should be managed. The setting is used only if list of managed namespaces is not empty.")
	command.Flags().StringVar(&opts.Name, "name", "", "Overwrite the cluster name")
	command.Flags().StringVar(&opts.Project, "project", "", "project of the cluster")
	command.Flags().Int64Var(&opts.Shard, "shard", -1, "Cluster shard number; inferred from hostname if not set")
	command.Flags().StringVar(&opts.ExecProviderCommand, "exec-command", "", "Command to run to provide client credentials to the cluster. You may need to build a custom ArgoCD image to ensure the command is available at runtime.")
	command.Flags().StringArrayVar(&opts.ExecProviderArgs, "exec-command-args", nil, "Arguments to supply to the --exec-command executable")
	command.Flags().StringToStringVar(&opts.ExecProviderEnv, "exec-command-env", nil, "Environment vars to set when running the --exec-command executable")
	command.Flags().StringVar(&opts.ExecProviderAPIVersion, "exec-command-api-version", "", "Preferred input version of the ExecInfo for the --exec-command executable")
	command.Flags().StringVar(&opts.ExecProviderInstallHint, "exec-command-install-hint", "", "Text shown to the user when the --exec-command executable doesn't seem to be present")
	command.Flags().StringVar(&opts.ClusterEndpoint, "cluster-endpoint", "", "Cluster endpoint to use. Can be one of the following: 'kubeconfig', 'kube-public', or 'internal'.")
	command.Flags().BoolVar(&opts.DisableCompression, "disable-compression", false, "Bypasses automatic GZip compression requests to the server")
}
