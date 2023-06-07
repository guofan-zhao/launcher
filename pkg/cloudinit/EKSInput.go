package cloudinit

/*
Copyright 2019 The Kubernetes Authors.

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

const (
	EKS = `{{.Header}}
{{template "files" .WriteFiles}}
-   path: /run/kubeadm/kubeadm.yaml
    owner: root:root
    permissions: '0640'
    content: |
      ---
{{.ClusterConfiguration | Indent 6}}
      ---
{{.InitConfiguration | Indent 6}}
-   path: /run/cluster-api/placeholder
    owner: root:root
    permissions: '0640'
    content: "This placeholder file is used to create the /run/cluster-api sub directory in a way that is compatible with both Linux and Windows (mkdir -p /run/cluster-api does not work with Windows)"
runcmd:
{{- template "commands" .PreKubeadmCommands }}
  - 'kubeadm init --config /run/kubeadm/kubeadm.yaml {{.KubeadmVerbosity}} && {{ .SentinelFileCommand }}'
{{- template "commands" .PostKubeadmCommands }}
{{- template "ntp" .NTP }}
{{- template "users" .Users }}
{{- template "disk_setup" .DiskSetup}}
{{- template "fs_setup" .DiskSetup}}
{{- template "mounts" .Mounts}}
`
)

// EKSInput defines the context to generate a eks instance user data.
type EKSInput struct {
	BaseUserData

	ClusterConfiguration string
	InitConfiguration    string
}

// NewEKS returns the user data string to be used on a eks instance.
func NewEKS(input *EKSInput) ([]byte, error) {
	input.Header = cloudConfigHeader
	input.WriteFiles = append(input.WriteFiles, input.AdditionalFiles...)
	userData, err := generate("InitEKS", EKS, input)
	if err != nil {
		return nil, err
	}

	return userData, nil
}