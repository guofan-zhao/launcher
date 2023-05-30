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

package controller

import (
	"context"
	ecnsv1 "easystack.com/plan/api/v1"
	"easystack.com/plan/pkg/cloud/service/provider"
	"easystack.com/plan/pkg/utils"
	errNew "errors"
	"fmt"
	clusteropenstackapis "github.com/easystack/cluster-api-provider-openstack/api/v1alpha6"
	"github.com/easystack/cluster-api-provider-openstack/pkg/scope"
	"github.com/easystack/cluster-api-provider-openstack/pkg/utils/errors"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/servergroups"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterapi "sigs.k8s.io/cluster-api/api/v1beta1"
	clusterkubeadm "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	clusterutils "sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sync"
)

const defaultopenstackadminconfsecret = "openstack-admin-etc"

// PlanReconciler reconciles a Plan object
type PlanReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

type MachineSetBind struct {
	ApiSet  *clusterapi.MachineSet      `json:"api_set"`
	PlanSet *ecnsv1.MachineSetReconcile `json:"plan_set"`
}

type PlanMachineSetBind struct {
	Plan *ecnsv1.Plan     `json:"plan"`
	Bind []MachineSetBind `json:"bind"`
}

//+kubebuilder:rbac:groups=ecns.easystack.com,resources=plans,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=ecns.easystack.com,resources=plans/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=ecns.easystack.com,resources=plans/finalizers,verbs=update
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=openstackclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=openstackclusters/status,verbs=get
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinesets;machinesets/status,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=openstackmachinetemplates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets;,verbs=get;create;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Plan object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.14.4/pkg/reconcile
func (r *PlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	// Fetch the OpenStackMachine instance.
	plan := &ecnsv1.Plan{}
	err := r.Client.Get(ctx, req.NamespacedName, plan)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	log = log.WithValues("plan", plan.Name)
	cluster, err := clusterutils.GetClusterByName(ctx, r.Client, plan.Spec.ClusterName, plan.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}

	if cluster == nil {
		log.Info("Cluster Controller has not yet set OwnerRef")
		return reconcile.Result{}, nil
	}

	log = log.WithValues("cluster", cluster.Name)
	if plan.Spec.Paused == true {
		// set cluster.Spec.Paused = true
		// first get the clusterv1.Cluster, then set cluster.Spec.Paused = true
		// then update the cluster
		// Fetch the Cluster.
		if cluster.Spec.Paused == true {
			log.Info("Cluster is already paused")
			return ctrl.Result{}, nil
		} else {
			cluster.Spec.Paused = true
			if err := r.Client.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}

		}

		return ctrl.Result{}, nil
	} else {
		if cluster.Spec.Paused == true {
			cluster.Spec.Paused = false
			if err := r.Client.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	patchHelper, err := patch.NewHelper(plan, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	osProviderClient, clientOpts, projectID, err := provider.NewClientFromSecret(ctx, r.Client, req.Namespace, defaultopenstackadminconfsecret, "default")
	if err != nil {
		return reconcile.Result{}, err
	}
	scope := &scope.Scope{
		ProviderClient:     osProviderClient,
		ProviderClientOpts: clientOpts,
		ProjectID:          projectID,
		Logger:             log,
	}

	if !plan.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, scope, patchHelper, plan)
	}

	// Handle non-deleted clusters
	return r.reconcileNormal(ctx, scope, patchHelper, plan)

}

func (r *PlanReconciler) reconcileNormal(ctx context.Context, scope *scope.Scope, patchHelper *patch.Helper, plan *ecnsv1.Plan) (_ ctrl.Result, reterr error) {
	// If the OpenStackMachine doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(plan, ecnsv1.MachineFinalizer)
	// Register the finalizer immediately to avoid orphaning plan resources on delete
	if err := patchHelper.Patch(ctx, plan); err != nil {
		return ctrl.Result{}, err
	}
	scope.Logger.Info("Reconciling plan openstack resource")
	// get gopher cloud client
	//create trust user
	// TODO get cluster uuid
	clusterUUID := ""
	// TODO get user id
	userID := ""
	err := syncTrustUser(ctx, scope, clusterUUID, userID)
	if err != nil {
		return ctrl.Result{}, err
	}
	// get or create sshkeys secret
	err = syncSSH(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	//TODO  get or create cluster.cluster.x-k8s.io
	err = syncCreateCluster(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	//TODO  get or create openstackcluster.infrastructure.cluster.x-k8s.io
	err = syncCreateOpenstackCluster(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	// TODO get or create openstack auth config secret
	err = syncOpenstackAuthConfig(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	//TODO  get or create KubeadmConfig ,no use
	err = syncCreateKubeadmConfig(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	//TODO  get or create server groups,master one,work one
	mastergroupID, nodegroupID, err := syncServerGroups(ctx, scope, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	// List all machineset for this plan
	machineSets, err := utils.ListMachineSets(ctx, r.Client, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(machineSets.Items) == 0 {
		// create all machineset Once
		for _, set := range plan.Spec.MachineSets {
			// create machineset
			err := utils.CreateMachineSet(ctx, scope, r.Client, plan, set, mastergroupID, nodegroupID)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
	} else {
		// skip create machineset

	}
	// Reconcile every machineset replicas
	err = r.syncMachine(ctx, scope, r.Client, plan, mastergroupID, nodegroupID)
	if err != nil {
		return ctrl.Result{}, err
	}
	// update plan status
	err = r.updateStatus(ctx, plan)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *PlanReconciler) reconcileDelete(ctx context.Context, scope *scope.Scope, patchHelper *patch.Helper, plan *ecnsv1.Plan) (_ ctrl.Result, reterr error) {
	return ctrl.Result{}, nil
}

// TODO sync trust user
func syncTrustUser(ctx context.Context, scope *scope.Scope, clusterUUID string, userID string) error {
	// TODO get trust cm by name  If not exist,then create trust user

	IdentityClient, err := openstack.NewIdentityV3(scope.ProviderClient, gophercloud.EndpointOpts{
		Region: scope.ProviderClientOpts.RegionName,
	})
	if err != nil {
		return err
	}
	_, err = utils.CreateTrustUser(ctx, IdentityClient, scope.ProjectID, clusterUUID, userID)

	return nil
}

// TODO sync openstack auth config
func syncOpenstackAuthConfig(ctx context.Context, client client.Client, plan *ecnsv1.Plan) error {
	// TODO get openstack auth config secret by name  If not exist,then create openstack auth config and its secret.
	// ...
	trust, err := utils.GetTrustUser(ctx, client, plan)
	if err != nil {
		return err
	}
	//check openstackcluster is ready by name ,if true, get or create openstack auth config
	ready, err := utils.CheckOpenstackClusterReady(ctx, client, plan)
	if err != nil {
		return err
	}
	if ready {
		err = utils.GetOrCreateOpenstackAuthConfig(ctx, client, plan, trust)
		if err != nil {
			return err
		}
	}
	return nil
}

// TODO sync create cluster
func syncCreateCluster(ctx context.Context, client client.Client, plan *ecnsv1.Plan) error {
	// TODO get cluster by name  If not exist,then create cluster
	cluster := clusterapi.Cluster{}
	err := client.Get(ctx, types.NamespacedName{Name: plan.Spec.ClusterName, Namespace: plan.Namespace}, &cluster)
	if err != nil {
		if errors.IsNotFound(err) {
			// TODO create cluster resource
			cluster.Name = plan.Spec.ClusterName
			cluster.Namespace = plan.Namespace
			cluster.Spec.ClusterNetwork.Pods.CIDRBlocks = []string{plan.Spec.PodCidr}
			cluster.Spec.ClusterNetwork.ServiceDomain = "cluster.local"
			cluster.Spec.InfrastructureRef.APIVersion = "infrastructure.cluster.x-k8s.io/v1alpha6"
			cluster.Spec.InfrastructureRef.Kind = "OpenStackCluster"
			cluster.Spec.InfrastructureRef.Name = plan.Spec.ClusterName
			err := client.Create(ctx, &cluster)
			if err != nil {
				return err
			}
		}
		return err
	}
	return nil
}

// Todo sync create openstackcluster
func syncCreateOpenstackCluster(ctx context.Context, client client.Client, plan *ecnsv1.Plan) error {
	//TODO get openstackcluster by name  If not exist,then create openstackcluster
	openstackCluster := clusteropenstackapis.OpenStackCluster{}
	err := client.Get(ctx, types.NamespacedName{Name: plan.Spec.ClusterName, Namespace: plan.Namespace}, &openstackCluster)
	if err != nil {
		if errors.IsNotFound(err) {
			// TODO create openstackcluster resource
			openstackCluster.Name = plan.Spec.ClusterName
			openstackCluster.Namespace = plan.Namespace
			openstackCluster.Spec.DisableAPIServerFloatingIP = false
			openstackCluster.Spec.APIServerLoadBalancer.Enabled = false
			openstackCluster.Spec.CloudName = plan.Spec.ClusterName
			openstackCluster.Spec.DNSNameservers = plan.Spec.DNSNameservers
			if plan.Spec.UseFloatIP == true {
				openstackCluster.Spec.ExternalNetworkID = plan.Spec.ExternalNetworkId
			} else {
				openstackCluster.Spec.ExternalNetworkID = ""
			}
			openstackCluster.Spec.ManagedSecurityGroups = true
			openstackCluster.Spec.NodeCIDR = plan.Spec.NodeCIDR
			openstackCluster.Spec.IdentityRef.Kind = "Secret"
			openstackCluster.Spec.IdentityRef.Name = plan.Spec.ClusterName
			err := client.Create(ctx, &openstackCluster)
			if err != nil {
				return err
			}
			return nil
		}
		return err
	}
	return nil

}

// TODO sync create kubeadmconfig
func syncCreateKubeadmConfig(ctx context.Context, client client.Client, plan *ecnsv1.Plan) error {
	//TODO get kubeadmconfig by name  If not exist,then create kubeadmconfig
	kubeadmconfigte := &clusterkubeadm.KubeadmConfigTemplate{}
	err := client.Get(ctx, types.NamespacedName{Name: plan.Spec.ClusterName, Namespace: plan.Namespace}, kubeadmconfigte)
	if err != nil {
		if errors.IsNotFound(err) {
			//TODO create kubeadmconfig resource
			kubeadmconfigte.Name = plan.Spec.ClusterName
			kubeadmconfigte.Namespace = plan.Namespace
			err := client.Create(ctx, kubeadmconfigte)
			if err != nil {
				return err
			}
			return nil
		}
	}
	return nil

}

// TODO sync ssh key
func syncSSH(ctx context.Context, client client.Client, plan *ecnsv1.Plan) error {
	// TODO get ssh secret by name  If not exist,then create ssh key
	_, _, err := utils.GetOrCreateSSHKeySecret(ctx, client, plan)
	if err != nil {
		return err
	}
	return nil
}

// TODO sync create  server group
func syncServerGroups(ctx context.Context, scope *scope.Scope, plan *ecnsv1.Plan) (string, string, error) {
	//TODO get server group by name  If not exist,then create server group
	// 1. get openstack client

	client, err := openstack.NewComputeV2(scope.ProviderClient, gophercloud.EndpointOpts{
		Region: scope.ProviderClientOpts.RegionName,
	})
	if err != nil {
		return "", "", err
	}
	//TODO check server group is exist or not create server group

	sg_master, err := servergroups.Create(client, &servergroups.CreateOpts{
		Name:     fmt.Sprintf("%s_%s", plan.Spec.ClusterName, "master"),
		Policies: []string{"anti-affinity"},
	}).Extract()
	if err != nil {
		return "", "", err

	}
	sg_work, err := servergroups.Create(client, &servergroups.CreateOpts{
		Name:     fmt.Sprintf("%s_%s", plan.Spec.ClusterName, "work"),
		Policies: []string{"anti-affinity"},
	}).Extract()
	if err != nil {
		return "", "", err
	}
	return sg_master.ID, sg_work.ID, nil

}

// TODO sync every machineset and other resource replicas to plan
func (r *PlanReconciler) syncMachine(ctx context.Context, sc *scope.Scope, cli client.Client, plan *ecnsv1.Plan, masterGroupID string, nodeGroupID string) error {
	// TODO get every machineset replicas to plan
	// 1. get machineset list
	labels := map[string]string{ecnsv1.MachineSetClusterLabelName: plan.Spec.ClusterName}
	machineSetList := &clusterapi.MachineSetList{}
	err := cli.List(ctx, machineSetList, client.InNamespace(plan.Namespace), client.MatchingLabels(labels))
	if err != nil {
		return err
	}
	if len(machineSetList.Items) != len(plan.Spec.MachineSets) {
		return fmt.Errorf("machineSetList length is not equal plan.Spec.MachineSets length")
	}
	var planBind = PlanMachineSetBind{}
	planBind.Plan = plan
	// 2. get every machineset replicas
	for _, PlanSet := range plan.Spec.MachineSets {
		setName := fmt.Sprintf("%s%s", plan.Spec.ClusterName, PlanSet.Role)
		for _, ApiSet := range machineSetList.Items {
			if ApiSet.Name == setName {
				planBind.Bind = append(planBind.Bind, MachineSetBind{
					ApiSet:  &ApiSet,
					PlanSet: PlanSet,
				})
			}

		}
	}
	// every ApiSet has one goroutine to scale replicas
	var wg sync.WaitGroup
	for _, bind := range planBind.Bind {
		if *bind.ApiSet.Spec.Replicas < bind.PlanSet.Replica {
			wg.Add(1)
			go func(ctxfake context.Context, scope *scope.Scope, c client.Client, target *ecnsv1.MachineSetReconcile, actual *clusterapi.MachineSet, totalplan *ecnsv1.Plan, wait *sync.WaitGroup, mastergroup string, nodegroup string) {
				err = r.processWork(ctxfake, scope, c, target, *actual, plan, wait, mastergroup, nodegroup)
				if err != nil {
					sc.Logger.Error(err, "sync machineSet replicas failed")
				}
			}(ctx, sc, cli, bind.PlanSet, bind.ApiSet, plan, &wg, masterGroupID, nodeGroupID)
		}
	}
	wg.Wait()

	return nil
}

// TODO  sync signal machineset replicas
func (r *PlanReconciler) processWork(ctx context.Context, sc *scope.Scope, c client.Client, target *ecnsv1.MachineSetReconcile, actual clusterapi.MachineSet, plan *ecnsv1.Plan, wait *sync.WaitGroup, mastergroup string, nodegroup string) error {
	defer func() {
		wait.Done()
	}()
loop:
	for {
		// get machineset status now
		var acNow clusterapi.MachineSet
		err := c.Get(ctx, types.NamespacedName{Name: actual.Name, Namespace: actual.Namespace}, &acNow)
		if err != nil {
			return err
		}
		diff := target.Replica - *acNow.Spec.Replicas
		switch {
		case diff == 0:
			break loop
		case diff > 0:
			index := *acNow.Spec.Replicas
			err := utils.AddReplicas(ctx, sc, c, target, acNow, plan, int(index), mastergroup, nodegroup)
			if err != nil {
				return err
			}
			continue
		case diff < 0:
			sc.Logger.Error(errNew.New("the actual replicas > plan replicas"), "cannot happend error")
			break loop
		}

	}
	return nil

}

// TODO update plan status
func (r *PlanReconciler) updateStatus(ctx context.Context, plan *ecnsv1.Plan) error {
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PlanReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ecnsv1.Plan{}).
		Complete(r)
}
