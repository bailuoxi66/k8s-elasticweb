# k8s-elasticweb
```bash
实现一个K8S Operator，通过自定义资源（CR）实现以下功能：
1. 通过Deployment部署指定的镜像，支持应用扩缩、资源Limit和Request；
2. 为应用创建LoadBalancer类型的Service，支持TCP和UDP协议，容器端口和服务端口配置；
3. 删除自定义资源时能够自动清除对应的Deployment和Service；

输出：
1. 一份CRD；
2. 一个Controller；
3. 一份用于在K8S中部署CRD和Controller，并为Controller配置相关权限的YAML文件；
4. 一份能够演示Operator所有功能的CR；


后续支持：思考一个问题，现在crd里面的当前qps是写死的，但真实情况下，数据肯定是来源于监控系统（对接prometheus或者arms），相当于手工实现一个hpa控制器
```



## 0. 基础资料

### 0.0 基础环境
- 源码：https://github.com/bailuoxi66/k8s-elasticweb
- go版本：go version go1.17.1 darwin/amd64
- operator-sdk：operator-sdk version: "v1.16.0", commit: "560044140c4f3d88677e4ef2872931f5bb97f255", kubernetes version: "v1.21", go version: "go1.17.5", GOOS: "darwin", GOARCH: "amd64" 【注意：使用operator-sdk创建operator的时候对版本要求没有kubebuilder高。但是建议尽量保持一致，我这里这样使用是因为现有其他版本，后来才使用到opearor-sdkls】
- docker-desktop：
![image](https://user-images.githubusercontent.com/35655760/156906090-dee5a9d4-e2bf-4550-9b8a-50d8dd6f52cd.png)


kustomize：{Version:kustomize/v4.5.1 GitCommit:746bd18a8c0ba5768b4519778c82a5fb3e667466 BuildDate:2022-02-02T19:02:13Z GoOs:darwin GoArch:amd64}

### 0.1 CRD
CRD设计之Spec部分
```bash
Image		# 镜像
NodePort    # Service Type是NodePort类型下的浏览器访问本地集群服务的端口
TargetPort  # Service 要转发的Pod的端口
PodPort     # Pod资源的端口
Port        # Service 提供的端口，可以转发到TagetPort上
SinglePodQPS   # 单个Pod的QPS上限
TotalQPS       # 当前整个业务需要的总QPS
Resources	# 资源限制
Protocol    # Service支持的协议类型
```
注意：上述的SinglePodQPS和TotalQPS是为了应用容器的扩容缩容
            NodePort在controller代码里面为NodePort时，才有意义
            Protocol：是为了设置当前的协议类型，支持TCP/UDP
            Image：指定要拉取的镜像
            Resources：指定Pod的资源限制
#### CRD设计之Status部分
● Status用来保存实际值，这里设计成只有一个字段realQPS，表示当前整个operator实际能支持的QPS
#### CRD逻辑设计部分
核心逻辑为：算出需要多少个pod，然后通过更新deployment让pod数量达到要求，在此核心的基础上再把创建deployment和service、更新status
![image](https://user-images.githubusercontent.com/35655760/156906160-b5983164-0070-40ed-9e75-7dd5fe6e73fa.png)


#### CRD源码
```bash
package v1

import (
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstr "k8s.io/apimachinery/pkg/util/intstr"
	"strconv"
)

// 期望状态
type ElasticWebSpec struct {
	// 业务服务对应的镜像，包括名称:tag
	Image string `json:"image"`

	NodePort   *int32              `json:"nodePort"`
	TargetPort *intstr.IntOrString `json:"targetPort"`
	PodPort    *int32              `json:"podPort"`
	Port       *int32              `json:"port"`

	// 单个pod的QPS上限
	SinglePodQPS *int32 `json:"singlePodQPS"`
	// 当前整个业务的总QPS
	TotalQPS *int32 `json:"totalQPS"`

	// 资源限制
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Service 支持的协议类型
	Protocol *corev1.Protocol `json:"protocol"`
}

// 实际状态，该数据结构中的值都是业务代码计算出来的
type ElasticWebStatus struct {
	// 当前kubernetes中实际支持的总QPS
	RealQPS *int32 `json:"realQPS"`
}

// +kubebuilder:object:root=true

// ElasticWeb is the Schema for the elasticwebs API
type ElasticWeb struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ElasticWebSpec   `json:"spec,omitempty"`
	Status ElasticWebStatus `json:"status,omitempty"`
}

func (in *ElasticWeb) String() string {
	var realQPS string

	if nil == in.Status.RealQPS {
		realQPS = "nil"
	} else {
		realQPS = strconv.Itoa(int(*(in.Status.RealQPS)))
	}

	return fmt.Sprintf("Image [%s], Port [%d], SinglePodQPS [%d], TotalQPS [%d], RealQPS [%s]",
		in.Spec.Image,
		*(in.Spec.Port),
		*(in.Spec.SinglePodQPS),
		*(in.Spec.TotalQPS),
		realQPS)
}

// +kubebuilder:object:root=true

// ElasticWebList contains a list of ElasticWeb
type ElasticWebList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ElasticWeb `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ElasticWeb{}, &ElasticWebList{})
}
```


#### 0.2 Controller
```bash
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
	"context"
	"fmt"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	elasticwebv1 "github.com/elasticweb/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (

	// tomcat容器的端口号
	CONTAINER_PORT = 8080
	// deployment中的APP标签名
	APP_NAME = "elastic-app"
	// 单个POD的CPU资源申请
	CPU_REQUEST = "100m"
	// 单个POD的CPU资源上限
	CPU_LIMIT = "100m"
	// 单个POD的内存资源申请
	MEM_REQUEST = "512Mi"
	// 单个POD的内存资源上限
	MEM_LIMIT = "512Mi"
)

// ElasticWebReconciler reconciles a ElasticWeb object
type ElasticWebReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ElasticWeb object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile

var globalLog = log.Log.WithName("global")

func (r *ElasticWebReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO(user): your logic here
	globalLog.Info("1. start reconcile logic")
	// 实例化数据结构
	instance := &elasticwebv1.ElasticWeb{}

	// 通过客户端工具查询，查询条件是
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {

		// 如果没有实例，就返回空结果，这样外部就不再立即调用Reconcile方法了
		if errors.IsNotFound(err) {
			globalLog.Info("2.1. instance not found, maybe removed")
			return reconcile.Result{}, nil
		}

		globalLog.Error(err, "2.2 error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	globalLog.Info("3. instance : " + instance.String())

	// 查找deployment
	deployment := &appsv1.Deployment{}

	// 用客户端工具查询
	err = r.Get(ctx, req.NamespacedName, deployment)

	// 查找时发生异常，以及查出来没有结果的处理逻辑
	if err != nil {
		// 如果没有实例就要创建了
		if errors.IsNotFound(err) {
			globalLog.Info("4. deployment not exists")

			// 如果对QPS没有需求，此时又没有deployment，就啥事都不做了
			if *(instance.Spec.TotalQPS) < 1 {
				globalLog.Info("5.1 not need deployment")
				// 返回
				return ctrl.Result{}, nil
			}

			// 先要创建service
			if err = createServiceIfNotExists(ctx, r, instance, req); err != nil {
				globalLog.Error(err, "5.2 error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 立即创建deployment
			if err = createDeployment(ctx, r, instance); err != nil {
				globalLog.Error(err, "5.3 error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 如果创建成功就更新状态
			if err = updateStatus(ctx, r, instance); err != nil {
				globalLog.Error(err, "5.4. error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 创建成功就可以返回了
			return ctrl.Result{}, nil
		} else {
			globalLog.Error(err, "7. error")
			// 返回错误信息给外部
			return ctrl.Result{}, err
		}
	}

	elasticWeb := *instance
	if !reflect.DeepEqual(deployment.Spec.Template.Spec.Containers[0].Resources, elasticWeb.Spec.Resources) {
		// Pod更新资源  每一次都进行更新
		globalLog.Info("8. update resources")
		deployment.Spec.Template.Spec.Containers[0].Resources = elasticWeb.Spec.Resources
		// 通过客户端更新deployment
		if err = r.Update(ctx, deployment); err != nil {
			globalLog.Error(err, "8. update deployment replicas error")
			// 返回错误信息给外部
			return ctrl.Result{}, err
		}
	}

	// 如果查到了deployment，并且没有返回错误，就走下面的逻辑
	// 根据单QPS和总QPS计算期望的副本数
	expectReplicas := getExpectReplicas(instance)

	// 当前deployment的期望副本数
	realReplicas := *deployment.Spec.Replicas

	globalLog.Info(fmt.Sprintf("9. expectReplicas [%d], realReplicas [%d]", expectReplicas, realReplicas))

	// 如果相等，就直接返回了
	if expectReplicas == realReplicas {
		globalLog.Info("10. return now")
		return ctrl.Result{}, nil
	}

	// 如果不等，就要调整
	*(deployment.Spec.Replicas) = expectReplicas

	globalLog.Info("11. update deployment's Replicas")
	// 通过客户端更新deployment
	if err = r.Update(ctx, deployment); err != nil {
		globalLog.Error(err, "12. update deployment replicas error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	globalLog.Info("13. update status")

	// 如果更新deployment的Replicas成功，就更新状态
	if err = updateStatus(ctx, r, instance); err != nil {
		globalLog.Error(err, "14. update status error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ElasticWebReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&elasticwebv1.ElasticWeb{}).
		Complete(r)
}

// 根据单个QPS和总QPS计算pod数量
func getExpectReplicas(elasticWeb *elasticwebv1.ElasticWeb) int32 {
	// 单个pod的QPS
	singlePodQPS := *(elasticWeb.Spec.SinglePodQPS)

	// 期望的总QPS
	totalQPS := *(elasticWeb.Spec.TotalQPS)

	// Replicas就是要创建的副本数
	replicas := totalQPS / singlePodQPS

	if totalQPS%singlePodQPS > 0 {
		replicas++
	}

	return replicas
}

// 新建service
func createServiceIfNotExists(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb, req ctrl.Request) error {
	service := &corev1.Service{}

	err := r.Get(ctx, req.NamespacedName, service)

	// 如果查询结果没有错误，证明service正常，就不做任何操作
	if err == nil {
		globalLog.Info("service exists")
		return nil
	}

	// 如果错误不是NotFound，就返回错误
	if !errors.IsNotFound(err) {
		globalLog.Error(err, "query service error")
		return err
	}

	// 实例化一个数据结构
	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: elasticWeb.Namespace,
			Name:      elasticWeb.Name,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Port:       *elasticWeb.Spec.Port,
				Protocol:   *elasticWeb.Spec.Protocol,
				TargetPort: *elasticWeb.Spec.TargetPort,
				NodePort:   *elasticWeb.Spec.NodePort,
			},
			},
			Selector: map[string]string{
				"app": APP_NAME,
			},
			//Type: "LoadBalancer",
			Type: "NodePort",
		},
	}

	// 这一步非常关键！
	// 建立关联后，删除elasticweb资源时就会将service也删除掉
	globalLog.Info("set reference")
	if err := controllerutil.SetControllerReference(elasticWeb, service, r.Scheme); err != nil {
		globalLog.Error(err, "SetControllerReference error")
		return err
	}

	// 创建service
	globalLog.Info("start create service")
	if err := r.Create(ctx, service); err != nil {
		globalLog.Error(err, "create service error")
		return err
	}

	globalLog.Info("create service success")
	return nil
}

// 新建deployment
func createDeployment(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb) error {

	// 计算期望的pod数量
	expectReplicas := getExpectReplicas(elasticWeb)

	globalLog.Info(fmt.Sprintf("expectReplicas [%d]", expectReplicas))

	// 实例化一个数据结构
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: elasticWeb.Namespace,
			Name:      elasticWeb.Name,
		},
		Spec: appsv1.DeploymentSpec{
			// 副本数是计算出来的
			Replicas: pointer.Int32Ptr(expectReplicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": APP_NAME,
				},
			},

			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": APP_NAME,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: APP_NAME,
							// 用指定的镜像
							Image:           elasticWeb.Spec.Image,
							ImagePullPolicy: "IfNotPresent",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolSCTP,
									ContainerPort: *elasticWeb.Spec.PodPort,
								},
							},
							Resources: elasticWeb.Spec.Resources,
						},
					},
				},
			},
		},
	}

	// 这一步非常关键！
	// 建立关联后，删除elasticweb资源时就会将deployment也删除掉
	globalLog.Info("set reference")
	if err := controllerutil.SetControllerReference(elasticWeb, deployment, r.Scheme); err != nil {
		globalLog.Error(err, "SetControllerReference error")
		return err
	}

	// 创建deployment
	globalLog.Info("start create deployment")
	if err := r.Create(ctx, deployment); err != nil {
		globalLog.Error(err, "create deployment error")
		return err
	}

	globalLog.Info("create deployment success")

	return nil
}

// 完成了pod的处理后，更新最新状态
func updateStatus(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb) error {

	// 单个pod的QPS
	singlePodQPS := *(elasticWeb.Spec.SinglePodQPS)

	// pod总数
	replicas := getExpectReplicas(elasticWeb)

	// 当pod创建完毕后，当前系统实际的QPS：单个pod的QPS * pod总数
	// 如果该字段还没有初始化，就先做初始化
	if nil == elasticWeb.Status.RealQPS {
		elasticWeb.Status.RealQPS = new(int32)
	}

	*(elasticWeb.Status.RealQPS) = singlePodQPS * replicas

	globalLog.Info(fmt.Sprintf("singlePodQPS [%d], replicas [%d], realQPS[%d]", singlePodQPS, replicas, *(elasticWeb.Status.RealQPS)))

	if err := r.Update(ctx, elasticWeb); err != nil {
		globalLog.Error(err, "update instance error")
		return err
	}

	return nil
}
```

#### 0.3 Controller权限
config/rbac/role.yaml
```bash
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  creationTimestamp: null
  name: manager-role
rules:
- apiGroups:
  - apps
  resources:
  - deployments
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - ""
  resources:
  - services
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - elasticweb.example.com
  resources:
  - elasticwebs
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
- apiGroups:
  - elasticweb.example.com
  resources:
  - elasticwebs/finalizers
  verbs:
  - update
- apiGroups:
  - elasticweb.example.com
  resources:
  - elasticwebs/status
  verbs:
  - get
  - patch
  - update
config/rbac/role_binding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: manager-rolebinding
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: manager-role
subjects:
- kind: ServiceAccount
  name: controller-manager
  namespace: system
```

#### 0.4 CR
```bash
apiVersion: v1
kind: Namespace
metadata:
  name: dev
  labels:
    name: dev
---


apiVersion: elasticweb.example.com/v1
kind: ElasticWeb
metadata:
  namespace: dev
  name: elasticweb-sample
spec:
  # TODO(user): Add fields here
  # Add fields here
  image: tomcat:8.0.18-jre8
  nodePort: 30004
  port: 8081
  targetPort: 8080
  podPort: 8080
  singlePodQPS: 500
  totalQPS: 700
  resources:
    limits:
      cpu: 400m
      memory: 500Mi
    requests:
      cpu: 100m
      memory: 100Mi
  protocol: TCP
```

#### 0.5 不同的controller镜像
本地验证使用：make deploy IMG=dockerhubyu/elasticweb:014    【NodePort】
公网验证使用：make deploy IMG=dockerhubyu/elasticweb:013    【LoadBalancer】

## 1. 本地/公网访问验证
1.1 本地模拟公网访问
部署打包好的controller镜像
![image](https://user-images.githubusercontent.com/35655760/156906250-06963b15-0e5d-49e2-a9b5-c99e8c81273e.png)
![image](https://user-images.githubusercontent.com/35655760/156906256-3d4c3da6-82f3-4be0-8f0a-9a8ff93bc54e.png)


发布yaml文件
![image](https://user-images.githubusercontent.com/35655760/156906254-18dbd057-0c9e-432e-b03c-dbba6a572d71.png)
![image](https://user-images.githubusercontent.com/35655760/156906258-f2256325-fdd1-4c21-88a5-b445aa61930b.png)


1.1.0 访问验证
上述的30004是NodePort场景下的转发端口。当前是LoadBalancer，所以可以直接使用
localhost:8080访问，用于验证服务是否正常
![image](https://user-images.githubusercontent.com/35655760/156906262-8ed67ea9-aa53-422a-9080-2d81e502613e.png)



查看日志【会发现确实启动了一个pod】
![image](https://user-images.githubusercontent.com/35655760/156906266-4b8e88bb-d0b2-4b7b-888e-1356c189875b.png)
![image](https://user-images.githubusercontent.com/35655760/156906269-26069d84-87e0-4ea3-bd95-00342c456d90.png)


1.1.1 扩容演示 - 修改totalQps，并且再次发布
因为总共需要支持600qps。单机只能支持500.所以需要两台pod
![image](https://user-images.githubusercontent.com/35655760/156906272-05829e93-66a9-4b90-acf5-27f53ecd9b9c.png)
![image](https://user-images.githubusercontent.com/35655760/156906278-85718181-0e2e-4f70-93c4-f952a419c5a4.png)
![image](https://user-images.githubusercontent.com/35655760/156906280-3f89bec5-71ac-4df8-bd89-8cd3f9d48cad.png)



1.1.2 缩容演示 - 修改totalQps，并且再次发布
![image](https://user-images.githubusercontent.com/35655760/156906285-823ebfd1-48b9-4b0d-9c78-a4ec85461238.png)
![image](https://user-images.githubusercontent.com/35655760/156906288-6b73e9fa-53da-47e5-bb5f-fad49fa16e6a.png)
![image](https://user-images.githubusercontent.com/35655760/156906291-fcdda71b-1da1-49b7-8874-21bd6662b1db.png)


1.2 Aliyun上申请的K8S集群-公网访问
因为需要外网访问。所以需要在aliyun上创建cluster集群。
![image](https://user-images.githubusercontent.com/35655760/156906293-906f1960-e8a9-4d85-b3ca-daa344c115c5.png)
![image](https://user-images.githubusercontent.com/35655760/156906294-efb3cfe6-0e62-4c5c-8da6-d5831b7908b6.png)
![image](https://user-images.githubusercontent.com/35655760/156906295-e0030c99-56d2-40cf-a2bd-f15de83c3889.png)



验证：使用提供的公网ip+端口
![image](https://user-images.githubusercontent.com/35655760/156906300-46e1344e-2c40-434f-9f8a-d8fd952b676c.png)

其他验证：跟本地一样
## 2. Service Protocol验证


TCP场景
![image](https://user-images.githubusercontent.com/35655760/156906595-2d4820d8-3782-435a-9a29-cc8a2fe033d9.png)


UDP场景
其中：端口验证使用nc确认
![image](https://user-images.githubusercontent.com/35655760/156906593-3e678922-bf90-41a4-a38d-af3dd80a5910.png)


## 3. 资源限制
启动Controller日志：


创建自定义文件
![image](https://user-images.githubusercontent.com/35655760/156906601-45621071-3ac9-4f32-9ded-3ae82c207961.png)


查看pod信息

```bash
➜ elasticweb kubectl get all -n dev
NAME                                    READY   STATUS    RESTARTS   AGE
pod/elasticweb-sample-744999557-l4pdd   1/1     Running   0          13s
pod/elasticweb-sample-744999557-p2tx7   1/1     Running   0          13s

NAME                        TYPE       CLUSTER-IP    EXTERNAL-IP   PORT(S)          AGE
service/elasticweb-sample   NodePort   10.99.8.113   <none>        8080:30004/TCP   13s

NAME                                READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/elasticweb-sample   2/2     2            2           13s

NAME                                          DESIRED   CURRENT   READY   AGE
replicaset.apps/elasticweb-sample-744999557   2         2         2       13s
➜ elasticweb kubectl get po elasticweb-sample-sample-744999557-l4pdd -n dev -o yaml
Error from server (NotFound): pods "elasticweb-sample-sample-744999557-l4pdd" not found
➜ elasticweb kubectl get po elasticweb-sample-744999557-l4pdd -n dev -o yaml
apiVersion: v1
kind: Pod
metadata:
  creationTimestamp: "2022-02-08T08:43:29Z"
  generateName: elasticweb-sample-744999557-
  labels:
    app: elastic-app
    pod-template-hash: "744999557"
  managedFields:
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:metadata:
        f:generateName: {}
        f:labels:
          .: {}
          f:app: {}
          f:pod-template-hash: {}
        f:ownerReferences:
          .: {}
          k:{"uid":"b1356745-2a5b-4ef1-952c-91d4f6990337"}: {}
      f:spec:
        f:containers:
          k:{"name":"elastic-app"}:
            .: {}
            f:image: {}
            f:imagePullPolicy: {}
            f:name: {}
            f:ports:
              .: {}
              k:{"containerPort":8080,"protocol":"SCTP"}:
                .: {}
                f:containerPort: {}
                f:name: {}
                f:protocol: {}
            f:resources:
              .: {}
              f:limits:
                .: {}
                f:cpu: {}
                f:memory: {}
              f:requests:
                .: {}
                f:cpu: {}
                f:memory: {}
            f:terminationMessagePath: {}
            f:terminationMessagePolicy: {}
        f:dnsPolicy: {}
        f:enableServiceLinks: {}
        f:restartPolicy: {}
        f:schedulerName: {}
        f:securityContext: {}
        f:terminationGracePeriodSeconds: {}
    manager: kube-controller-manager
    operation: Update
    time: "2022-02-08T08:43:29Z"
  - apiVersion: v1
    fieldsType: FieldsV1
    fieldsV1:
      f:status:
        f:conditions:
          k:{"type":"ContainersReady"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:status: {}
            f:type: {}
          k:{"type":"Initialized"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:status: {}
            f:type: {}
          k:{"type":"Ready"}:
            .: {}
            f:lastProbeTime: {}
            f:lastTransitionTime: {}
            f:status: {}
            f:type: {}
        f:containerStatuses: {}
        f:hostIP: {}
        f:phase: {}
        f:podIP: {}
        f:podIPs:
          .: {}
          k:{"ip":"10.1.0.38"}:
            .: {}
            f:ip: {}
        f:startTime: {}
    manager: kubelet
    operation: Update
    subresource: status
    time: "2022-02-08T08:43:31Z"
  name: elasticweb-sample-744999557-l4pdd
  namespace: dev
  ownerReferences:
  - apiVersion: apps/v1
    blockOwnerDeletion: true
    controller: true
    kind: ReplicaSet
    name: elasticweb-sample-744999557
    uid: b1356745-2a5b-4ef1-952c-91d4f6990337
  resourceVersion: "31757"
  uid: 685886c0-1f91-4fbd-b544-8dc210355d05
spec:
  containers:
  - image: tomcat:8.0.18-jre8
    imagePullPolicy: IfNotPresent
    name: elastic-app
    ports:
    - containerPort: 8080
      name: http
      protocol: SCTP
    resources:
      limits:
        cpu: 500m
        memory: 500Mi
      requests:
        cpu: 100m
        memory: 100Mi
    terminationMessagePath: /dev/termination-log
    terminationMessagePolicy: File
    volumeMounts:
    - mountPath: /var/run/secrets/kubernetes.io/serviceaccount
      name: kube-api-access-g9bp2
      readOnly: true
  dnsPolicy: ClusterFirst
  enableServiceLinks: true
  nodeName: docker-desktop
  preemptionPolicy: PreemptLowerPriority
  priority: 0
  restartPolicy: Always
  schedulerName: default-scheduler
  securityContext: {}
  serviceAccount: default
  serviceAccountName: default
  terminationGracePeriodSeconds: 30
  tolerations:
  - effect: NoExecute
    key: node.kubernetes.io/not-ready
    operator: Exists
    tolerationSeconds: 300
  - effect: NoExecute
    key: node.kubernetes.io/unreachable
    operator: Exists
    tolerationSeconds: 300
  volumes:
  - name: kube-api-access-g9bp2
    projected:
      defaultMode: 420
      sources:
      - serviceAccountToken:
          expirationSeconds: 3607
          path: token
      - configMap:
          items:
          - key: ca.crt
            path: ca.crt
          name: kube-root-ca.crt
      - downwardAPI:
          items:
          - fieldRef:
              apiVersion: v1
              fieldPath: metadata.namespace
            path: namespace
status:
  conditions:
  - lastProbeTime: null
    lastTransitionTime: "2022-02-08T08:43:29Z"
    status: "True"
    type: Initialized
  - lastProbeTime: null
    lastTransitionTime: "2022-02-08T08:43:31Z"
    status: "True"
    type: Ready
  - lastProbeTime: null
    lastTransitionTime: "2022-02-08T08:43:31Z"
    status: "True"
    type: ContainersReady
  - lastProbeTime: null
    lastTransitionTime: "2022-02-08T08:43:29Z"
    status: "True"
    type: PodScheduled
  containerStatuses:
  - containerID: docker://666c10276b71ef15f42714dee40ba3706110e7f0e451ddc5d7b78df11b7cc615
    image: tomcat:8.0.18-jre8
    imageID: docker-pullable://tomcat@sha256:ee021c24d38b6b465b35b9a3f39659ab02b0aa68957bce6713200325bb39e34a
    lastState: {}
    name: elastic-app
    ready: true
    restartCount: 0
    started: true
    state:
      running:
        startedAt: "2022-02-08T08:43:30Z"
  hostIP: 192.168.65.4
  phase: Running
  podIP: 10.1.0.38
  podIPs:
  - ip: 10.1.0.38
  qosClass: Burstable
  startTime: "2022-02-08T08:43:29Z"
```
如下：创建pod的时候，从指定文件填充
![image](https://user-images.githubusercontent.com/35655760/156906627-23795b4f-cb26-49f2-b4d8-a0675a9f0440.png)


3.1 实时更新资源
![image](https://user-images.githubusercontent.com/35655760/156906637-301cba95-d0a7-4c90-bc78-5c394808c058.png)
启动日志：可以发现启动的时候会比较资源。一致，所以不需要更新
![image](https://user-images.githubusercontent.com/35655760/156906640-ba6822af-c844-4d86-9852-d5f4a9f96673.png)
![image](https://user-images.githubusercontent.com/35655760/156906646-dae387c6-e757-48bb-91b7-4a7ce33b434c.png)
![image](https://user-images.githubusercontent.com/35655760/156906652-e6ed5ba2-e8a3-49e4-bfc9-aa4e51e15225.png)




接下来更新一下：【500-》800】
![image](https://user-images.githubusercontent.com/35655760/156906655-f113696f-edec-48ad-8130-b910b3c56b28.png)
![image](https://user-images.githubusercontent.com/35655760/156906658-ad5162b2-fcdc-4a6e-92dd-3fd4385b67d0.png)
![image](https://user-images.githubusercontent.com/35655760/156906665-3c0165a9-0c80-4c04-85ce-ffb2d6b352e0.png)
![image](https://user-images.githubusercontent.com/35655760/156906670-5fb5626b-d98b-4660-b3ad-4d1249198c13.png)



```bash
➜ elasticweb kubectl describe po elasticweb-sample-6c94798f95-lt2dm -n dev
Name:         elasticweb-sample-6c94798f95-lt2dm
Namespace:    dev
Priority:     0
Node:         docker-desktop/192.168.65.4
Start Time:   Thu, 10 Feb 2022 18:13:11 +0800
Labels:       app=elastic-app
              pod-template-hash=6c94798f95
Annotations:  <none>
Status:       Running
IP:           10.1.0.108
IPs:
  IP:           10.1.0.108
Controlled By:  ReplicaSet/elasticweb-sample-6c94798f95
Containers:
  elastic-app:
    Container ID:   docker://fbbdd0b0fe55ec6a89a5deb1f681a9fc8f70443438d5477120c4eadfdde2fbf9
    Image:          tomcat:8.0.18-jre8
    Image ID:       docker-pullable://tomcat@sha256:ee021c24d38b6b465b35b9a3f39659ab02b0aa68957bce6713200325bb39e34a
    Port:           8080/SCTP
    Host Port:      0/SCTP
    State:          Running
      Started:      Thu, 10 Feb 2022 18:13:12 +0800
    Ready:          True
    Restart Count:  0
    Limits:
      cpu:     800m
      memory:  500Mi
    Requests:
      cpu:        100m
      memory:     100Mi
    Environment:  <none>
    Mounts:
      /var/run/secrets/kubernetes.io/serviceaccount from kube-api-access-bh26q (ro)
Conditions:
  Type              Status
  Initialized       True
  Ready             True
  ContainersReady   True
  PodScheduled      True
Volumes:
  kube-api-access-bh26q:
    Type:                    Projected (a volume that contains injected data from multiple sources)
    TokenExpirationSeconds:  3607
    ConfigMapName:           kube-root-ca.crt
    ConfigMapOptional:       <nil>
    DownwardAPI:             true
QoS Class:                   Burstable
Node-Selectors:              <none>
Tolerations:                 node.kubernetes.io/not-ready:NoExecute op=Exists for 300s
                             node.kubernetes.io/unreachable:NoExecute op=Exists for 300s
Events:
  Type    Reason     Age   From               Message
  ----    ------     ----  ----               -------
  Normal  Scheduled  56s   default-scheduler  Successfully assigned dev/elasticweb-sample-6c94798f95-lt2dm to docker-desktop
  Normal  Pulled     56s   kubelet            Container image "tomcat:8.0.18-jre8" already present on machine
  Normal  Created    55s   kubelet            Created container elastic-app
  Normal  Started    55s   kubelet            Started container elastic-app
```



## 4. 端口修改【LoadBalancer下】
### 4.1 修改端口8080-》8081
![image](https://user-images.githubusercontent.com/35655760/156906687-5e2ab299-d1e3-4b59-bce0-e8c98065d128.png)


### 4.2 发布验证
![image](https://user-images.githubusercontent.com/35655760/156906695-b9a5af9b-7e28-42d3-a169-18c1a32887ca.png)
![image](https://user-images.githubusercontent.com/35655760/156906704-b689f9ec-39ba-462e-a2df-5f1cfe74660b.png)
![image](https://user-images.githubusercontent.com/35655760/156906712-9b2edf5b-0754-480c-bb97-4ada1f3b73ea.png)




## 5. Control 权限控制
API Server作为Kubernetes网关，是访问和管理资源对象的唯一入口，其各种集群组件访问资源都需要经过网关才能进行正常访问和管理。每一次的访问请求都需要进行合法性的检验，其中包括身份验证、操作权限验证以及操作规范验证等
```bash
权限管控通过：ClusterRole和ClusterRoleBinding资源将权限与ServiceAccount关联起来。
Operator必须拥有权限来get、list、create、watch需要的资源
operator-sdk和kubebuilder框架可以提供这些权限，通过注解为每个控制器分配权限
```

elasticweb/controllers/elasticweb_controller.go   里面的注解
![image](https://user-images.githubusercontent.com/35655760/156906741-dd4c2992-008c-48bf-ba1f-32b69afdecd9.png)
![image](https://user-images.githubusercontent.com/35655760/156906745-c10405d2-c8e6-4595-b6fb-d9b0e07bf4d5.png)
![image](https://user-images.githubusercontent.com/35655760/156906749-3e06b607-8481-4fdb-ae1b-6a1977d97196.png)




### SA
https://www.jianshu.com/p/0c5aa88420e2
k8s: 外部账户【User Accounts】
        内部账户【Service Accounts】
Service account是为了方便Pod里面的进程调用Kubernetes API或其他外部服务而设计的

可以新建SA。同时可以指定Image pull secrets

### RBAC
让一个用户（Users）扮演一个角色（Role），角色拥有权限，从而让用户拥有这样的权限，随后在授权机制当中，只需要将权限授予某个角色，此时用户将获取对应角色的权限，从而实现角色的访问控制

![image](https://user-images.githubusercontent.com/35655760/156906759-0df040a2-23ad-4137-a9e2-c558650efe49.png)

注：其中webhook也可以起“填充、校验”的功能，根据需要确认是否使用

## 6. Prometheus

因为当前使用的docker-desktop 部署的版本的1.22+的。但是很多会跟在高版本已经弃用了。
所以当前这里会选用minikube，因为可以指定具体的 版本。同时结合helm可以快速的实现prometheus的部署
### 6.0 minikube 启动 kubernetes【指定一个低版本】
```bash
minikube start --image-mirror-country='cn' --nodes 2 --kubernetes-version=v1.20.3
minikube status
minikube stop
```
![image](https://user-images.githubusercontent.com/35655760/156906780-cb03a59d-2a36-46d3-98dc-b08715c8ea72.png)


### 6.1 安装prometheus
```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
helm install prometheus prometheus-community/kube-prometheus-stack
```
![image](https://user-images.githubusercontent.com/35655760/156906788-cf869593-5bfa-4ef0-9a06-1db5370605de.png)
![image](https://user-images.githubusercontent.com/35655760/156906791-8aa2ea73-38c4-47df-a503-6bcacf04f7ab.png)


```bash
➜ elasticweb kubectl get all
NAME                                                         READY   STATUS    RESTARTS   AGE
pod/alertmanager-prometheus-kube-prometheus-alertmanager-0   2/2     Running   0          106s
pod/prometheus-grafana-7b6fb57f77-dd6g8                      3/3     Running   0          113s
pod/prometheus-kube-prometheus-operator-788dfbdccc-jmzst     1/1     Running   0          113s
pod/prometheus-kube-state-metrics-66ddfc9b97-6xxcc           1/1     Running   0          113s
pod/prometheus-prometheus-kube-prometheus-prometheus-0       2/2     Running   0          101s
pod/prometheus-prometheus-node-exporter-pg9x5                1/1     Running   0          113s

NAME                                              TYPE        CLUSTER-IP       EXTERNAL-IP   PORT(S)                      AGE
service/alertmanager-operated                     ClusterIP   None             <none>        9093/TCP,9094/TCP,9094/UDP   106s
service/kubernetes                                ClusterIP   10.96.0.1        <none>        443/TCP                      4m53s
service/prometheus-grafana                        ClusterIP   10.102.49.31     <none>        80/TCP                       114s
service/prometheus-kube-prometheus-alertmanager   ClusterIP   10.109.200.5     <none>        9093/TCP                     114s
service/prometheus-kube-prometheus-operator       ClusterIP   10.101.21.76     <none>        443/TCP                      114s
service/prometheus-kube-prometheus-prometheus     ClusterIP   10.101.230.154   <none>        9090/TCP                     113s
service/prometheus-kube-state-metrics             ClusterIP   10.96.130.4      <none>        8080/TCP                     114s
service/prometheus-operated                       ClusterIP   None             <none>        9090/TCP                     101s
service/prometheus-prometheus-node-exporter       ClusterIP   10.100.180.64    <none>        9100/TCP                     113s

NAME                                                 DESIRED   CURRENT   READY   UP-TO-DATE   AVAILABLE   NODE SELECTOR   AGE
daemonset.apps/prometheus-prometheus-node-exporter   1         1         1       1            1           <none>          113s

NAME                                                  READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/prometheus-grafana                    1/1     1            1           113s
deployment.apps/prometheus-kube-prometheus-operator   1/1     1            1           113s
deployment.apps/prometheus-kube-state-metrics         1/1     1            1           113s

NAME                                                             DESIRED   CURRENT   READY   AGE
replicaset.apps/prometheus-grafana-7b6fb57f77                    1         1         1       113s
replicaset.apps/prometheus-kube-prometheus-operator-788dfbdccc   1         1         1       113s
replicaset.apps/prometheus-kube-state-metrics-66ddfc9b97         1         1         1       113s

NAME                                                                    READY   AGE
statefulset.apps/alertmanager-prometheus-kube-prometheus-alertmanager   1/1     106s
statefulset.apps/prometheus-prometheus-kube-prometheus-prometheus       1/1     101s
```

### 6.2 Prometheus内置浏览器验证
kubectl port-forward $(kubectl get pods --selector "app.kubernetes.io/name=prometheus" --output=name) 9090
![image](https://user-images.githubusercontent.com/35655760/156906815-950d04a1-71f4-45c3-b80c-59e363e68940.png)

kubectl port-forward $(kubectl get pods --selector "app.kubernetes.io/name=grafana" --output=name) 3000
![image](https://user-images.githubusercontent.com/35655760/156906818-20634d8d-691e-4b57-ac3e-3043919f58b1.png)

【admin/prom-operator】
### 6.3 grafana监控业务pod
![image](https://user-images.githubusercontent.com/35655760/156906826-0f27ce1e-9271-43b0-92b7-76e38c761360.png)

进行缩容变化，并且查看grafana【也就是如下：可以查到当前使用的cpu约为0.75%】
![image](https://user-images.githubusercontent.com/35655760/156906829-3b73f3f6-e105-498f-87a4-0a404680f49d.png)

6.4 如何监控具体的指标？
code: git@github.com:bailuoxi66/k8s-elasticweb.git


1. 首先需要替换之前的镜像。因为如果要引入相应的指标需要SpringBoot项目
【暂时理解：参考https://mp.weixin.qq.com/s/0l5QY-jKxnIYjr8HriML8g】
2. 比如发布ServiceMonitor.yml 且务必保持labels一致
3. 有了对应的指标后。如何监控qps？我们有当前的访问总量，那么换一种思路【做差法可以解决这里的问题】
4. 找到对应的Http请求方式。然后完善go代码。同时需要解析最终的需要的结果【因为如下value里面的结果是有int，有string，所以解析的过程需要额外注意。这里的做法是进行替换，统一转化为string处理】
```bash
{
    "status":"success",
    "data":{
        "resultType":"vector",
        "result":[
            {
                "metric":{
                    "__name__":"http_server_requests_seconds_count",
                    "application":"expose-prometheus-demo",
                    "container":"elasticweb-sample",
                    "endpoint":"metric-traffic",
                    "exception":"None",
                    "instance":"172.17.0.7:8090",
                    "job":"elasticweb-sample",
                    "method":"GET",
                    "namespace":"default",
                    "outcome":"SUCCESS",
                    "pod":"elasticweb-sample-75bb5c47c7-9kbgp",
                    "service":"elasticweb-sample",
                    "status":"200",
                    "uri":"/"
                },
                "value":[
                    1645010854,
                    "12001"
                ]
            }
        ]
    }
}
```
5. 特殊情况下会产生panic。这里需要异常捕获【recover】
6. 使用goroutine的方式，结合Timer定时器，来保证定期执行
7. 为了避免多次产生新的goroutine。需要将逻辑放置在init里面
8. 基于go-stress-testing来获取最合理最真实的结果，如：单机qps等
9. 为了更好的更新，这里采用go执行系统命令的方式，来达到更新的目的
10. 手动监控当前的服务。确认数量是否变化

```bash
controllers/elasticweb_controller.go
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
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os/exec"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"

	elasticwebv1 "github.com/elasticweb/api/v1"
	"k8s.io/apimachinery/pkg/api/errors"
)

const (

	// tomcat容器的端口号
	CONTAINER_PORT = 8080
	// deployment中的APP标签名
	APP_NAME = "elasticweb-sample"
	// 单个POD的CPU资源申请
	CPU_REQUEST = "100m"
	// 单个POD的CPU资源上限
	CPU_LIMIT = "100m"
	// 单个POD的内存资源申请
	MEM_REQUEST = "512Mi"
	// 单个POD的内存资源上限
	MEM_LIMIT = "512Mi"

	TIME = 30
)

type Res struct {
	Status string `json:"status"`
	Data   Datas  `json:"data"`
}
type Datas struct {
	ResultType string        `json:"resultType"`
	Result     []ValueMetric `json:"result"`
}

type ValueMetric struct {
	Value []string `json:"value"`
}

// ElasticWebReconciler reconciles a ElasticWeb object
type ElasticWebReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=elasticweb.example.com,resources=elasticwebs/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the ElasticWeb object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile

var globalLog = log.Log.WithName("global")

func init() {
	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
		}
	}()
	go timer()
}

func (r *ElasticWebReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)

	// TODO(user): your logic here
	globalLog.Info("1. start reconcile logic")
	// 实例化数据结构
	instance := &elasticwebv1.ElasticWeb{}

	// 通过客户端工具查询，查询条件是
	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {

		// 如果没有实例，就返回空结果，这样外部就不再立即调用Reconcile方法了
		if errors.IsNotFound(err) {
			globalLog.Info("2.1. instance not found, maybe removed")
			return reconcile.Result{}, nil
		}

		globalLog.Error(err, "2.2 error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	globalLog.Info("3. instance : " + instance.String())

	// 查找deployment
	deployment := &appsv1.Deployment{}

	// 用客户端工具查询
	err = r.Get(ctx, req.NamespacedName, deployment)

	// 查找时发生异常，以及查出来没有结果的处理逻辑
	if err != nil {
		// 如果没有实例就要创建了
		if errors.IsNotFound(err) {
			globalLog.Info("4. deployment not exists")

			// 如果对QPS没有需求，此时又没有deployment，就啥事都不做了
			if *(instance.Spec.TotalQPS) < 1 {
				globalLog.Info("5.1 not need deployment")
				// 返回
				return ctrl.Result{}, nil
			}

			// 先要创建service
			if err = createServiceIfNotExists(ctx, r, instance, req); err != nil {
				globalLog.Error(err, "5.2 error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 立即创建deployment
			if err = createDeployment(ctx, r, instance); err != nil {
				globalLog.Error(err, "5.3 error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 如果创建成功就更新状态
			if err = updateStatus(ctx, r, instance); err != nil {
				globalLog.Error(err, "5.4. error")
				// 返回错误信息给外部
				return ctrl.Result{}, err
			}

			// 创建成功就可以返回了
			return ctrl.Result{}, nil
		} else {
			globalLog.Error(err, "7. error")
			// 返回错误信息给外部
			return ctrl.Result{}, err
		}
	}

	elasticWeb := *instance
	if !reflect.DeepEqual(deployment.Spec.Template.Spec.Containers[0].Resources, elasticWeb.Spec.Resources) {
		// Pod更新资源  每一次都进行更新
		globalLog.Info("8. update resources")
		deployment.Spec.Template.Spec.Containers[0].Resources = elasticWeb.Spec.Resources
		// 通过客户端更新deployment
		if err = r.Update(ctx, deployment); err != nil {
			globalLog.Error(err, "8. update deployment replicas error")
			// 返回错误信息给外部
			return ctrl.Result{}, err
		}
	}

	// 如果查到了deployment，并且没有返回错误，就走下面的逻辑
	// 根据单QPS和总QPS计算期望的副本数
	expectReplicas := getExpectReplicas(instance)

	// 当前deployment的期望副本数
	realReplicas := *deployment.Spec.Replicas

	globalLog.Info(fmt.Sprintf("9. expectReplicas [%d], realReplicas [%d]", expectReplicas, realReplicas))

	// 如果相等，就直接返回了
	if expectReplicas == realReplicas {
		globalLog.Info("10. return now")
		return ctrl.Result{}, nil
	}

	// 如果不等，就要调整
	*(deployment.Spec.Replicas) = expectReplicas

	globalLog.Info("11. update deployment's Replicas")
	// 通过客户端更新deployment
	if err = r.Update(ctx, deployment); err != nil {
		globalLog.Error(err, "12. update deployment replicas error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	globalLog.Info("13. update status")

	// 如果更新deployment的Replicas成功，就更新状态
	if err = updateStatus(ctx, r, instance); err != nil {
		globalLog.Error(err, "14. update status error")
		// 返回错误信息给外部
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ElasticWebReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&elasticwebv1.ElasticWeb{}).
		Complete(r)
}

// 根据单个QPS和总QPS计算pod数量
func getExpectReplicas(elasticWeb *elasticwebv1.ElasticWeb) int32 {
	// 单个pod的QPS
	singlePodQPS := *(elasticWeb.Spec.SinglePodQPS)

	// 期望的总QPS
	totalQPS := *(elasticWeb.Spec.TotalQPS)

	// Replicas就是要创建的副本数
	replicas := totalQPS / singlePodQPS

	if totalQPS%singlePodQPS > 0 {
		replicas++
	}

	return replicas
}

// 新建service
func createServiceIfNotExists(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb, req ctrl.Request) error {
	service := &corev1.Service{}

	err := r.Get(ctx, req.NamespacedName, service)

	// 如果查询结果没有错误，证明service正常，就不做任何操作
	if err == nil {
		globalLog.Info("service exists")
		return nil
	}

	// 如果错误不是NotFound，就返回错误
	if !errors.IsNotFound(err) {
		globalLog.Error(err, "query service error")
		return err
	}

	// 实例化一个数据结构
	// 初始化 + 赋值一体化
	m := map[string]string{
		"release": "prometheus",
		"app":     "elasticweb-sample",
	}

	service = &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: elasticWeb.Namespace,
			Name:      elasticWeb.Name,
			Labels:    m,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{{
				Port:       8080,
				Protocol:   "TCP",
				TargetPort: *elasticWeb.Spec.TargetPort,
				Name:       "http-traffic",
			}, {
				Port:       8090,
				Protocol:   "TCP",
				TargetPort: *elasticWeb.Spec.MTargetPort,
				Name:       "metric-traffic",
			},
			},
			Selector: map[string]string{
				"app": APP_NAME,
			},
		},
	}

	// 这一步非常关键！
	// 建立关联后，删除elasticweb资源时就会将service也删除掉
	globalLog.Info("set reference")
	if err := controllerutil.SetControllerReference(elasticWeb, service, r.Scheme); err != nil {
		globalLog.Error(err, "SetControllerReference error")
		return err
	}

	// 创建service
	globalLog.Info("start create service")
	if err := r.Create(ctx, service); err != nil {
		globalLog.Error(err, "create service error")
		return err
	}

	globalLog.Info("create service success")
	return nil
}

// 新建deployment
func createDeployment(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb) error {

	// 计算期望的pod数量
	expectReplicas := getExpectReplicas(elasticWeb)

	globalLog.Info(fmt.Sprintf("expectReplicas [%d]", expectReplicas))

	// 实例化一个数据结构
	m := map[string]string{
		"app": "elasticweb-sample",
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: elasticWeb.Namespace,
			Name:      elasticWeb.Name,
			Labels:    m,
		},
		Spec: appsv1.DeploymentSpec{
			// 副本数是计算出来的
			Replicas: pointer.Int32Ptr(expectReplicas),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": APP_NAME,
				},
			},

			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app": APP_NAME,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: APP_NAME,
							// 用指定的镜像
							Image:           elasticWeb.Spec.Image,
							ImagePullPolicy: "IfNotPresent",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolSCTP,
									ContainerPort: *elasticWeb.Spec.PodPort,
								},
								{
									ContainerPort: 8090,
								},
							},
							Resources: elasticWeb.Spec.Resources,
						},
					},
				},
			},
		},
	}

	// 这一步非常关键！
	// 建立关联后，删除elasticweb资源时就会将deployment也删除掉
	globalLog.Info("set reference")
	if err := controllerutil.SetControllerReference(elasticWeb, deployment, r.Scheme); err != nil {
		globalLog.Error(err, "SetControllerReference error")
		return err
	}

	// 创建deployment
	globalLog.Info("start create deployment")
	if err := r.Create(ctx, deployment); err != nil {
		globalLog.Error(err, "create deployment error")
		return err
	}

	globalLog.Info("create deployment success")

	return nil
}

// 完成了pod的处理后，更新最新状态
func updateStatus(ctx context.Context, r *ElasticWebReconciler, elasticWeb *elasticwebv1.ElasticWeb) error {

	// 单个pod的QPS
	singlePodQPS := *(elasticWeb.Spec.SinglePodQPS)

	// pod总数
	replicas := getExpectReplicas(elasticWeb)

	// 当pod创建完毕后，当前系统实际的QPS：单个pod的QPS * pod总数
	// 如果该字段还没有初始化，就先做初始化
	if nil == elasticWeb.Status.RealQPS {
		elasticWeb.Status.RealQPS = new(int32)
	}

	*(elasticWeb.Status.RealQPS) = singlePodQPS * replicas

	globalLog.Info(fmt.Sprintf("singlePodQPS [%d], replicas [%d], realQPS[%d]", singlePodQPS, replicas, *(elasticWeb.Status.RealQPS)))

	if err := r.Update(ctx, elasticWeb); err != nil {
		globalLog.Error(err, "update instance error")
		return err
	}

	return nil
}

func timer() {
	t := time.NewTicker(TIME * time.Second)
	defer t.Stop()
	time.Sleep(TIME * time.Second)

	for {
		select {
		case <-t.C:
			execSys()
		}
	}
}

func ExecCommand(strCommand string) string {
	cmd := exec.Command("/bin/bash", "-c", strCommand)

	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		fmt.Println("Execute failed when Start:" + err.Error())
		return ""
	}

	out_bytes, _ := ioutil.ReadAll(stdout)
	stdout.Close()

	if err := cmd.Wait(); err != nil {
		fmt.Println("Execute failed when Wait:" + err.Error())
		return ""
	}
	return string(out_bytes)
}

func execSys() {
	curTotal := curRes()

	if curTotal <= 0 {
		curTotal = 1
	}
	str := "config/samples/elasticweb_v1_elasticweb.yaml"
	s := "sed -i \"\" 's/totalQPS: 10/totalQPS: " + strconv.FormatInt(curTotal, 10) + "/g' " + str

	fmt.Println(s)
	s = ExecCommand(s)
	s1 := "kubectl apply -f config/samples/elasticweb_v1_elasticweb.yaml"

	s1 = ExecCommand(s1)
	fmt.Println(s)
	fmt.Println(s1)
	s2 := "sed -i \"\" 's/totalQPS: " + strconv.FormatInt(curTotal, 10) + "/totalQPS: 10" + "/g' " + str
	fmt.Println(s2)
	s2 = ExecCommand(s2)
}

func curRes() int64 {
	timeUnix := time.Now().Unix()
	preVal := getTotal(timeUnix - 30)
	val := getTotal(timeUnix)

	cur, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		fmt.Println("strconv.ParseInt is error")
	}
	preCur, err := strconv.ParseInt(preVal, 10, 64)
	if err != nil {
		fmt.Println("strconv.ParseInt is error")
	}
	cur = (cur - preCur) / 30

	fmt.Printf("%d", cur)
	return cur
}

func getTotal(timeUnix int64) string {

	defer func() {
		if err := recover(); err != nil {
		}
	}()

	curTIME := strconv.FormatInt(timeUnix, 10)
	Req := "http://localhost:9090/api/v1/query?query=http_server_requests_seconds_count&time="
	Req += curTIME
	fmt.Printf(Req)
	resp, err := http.Get(Req)
	if err != nil {
		globalLog.Info("http.Get is failure")
		return "0"
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	bodys := string(body)
	strReplaceAll := strings.ReplaceAll(bodys, curTIME, "\""+curTIME+"\"")
	var r Res
	err1 := json.Unmarshal([]byte(strReplaceAll), &r)
	if err1 != nil {
		globalLog.Error(err, "json.Unmarshal is failure")
		return "0"
	}
	fmt.Println(r)
	if r.Data.Result == nil || len(r.Data.Result) <= 0 {
		globalLog.Info("r.Data is nil or r.Data is Empty")
		return "0"
	}
	fmt.Println("type:", r.Data.Result[0].Value[1])

	return r.Data.Result[0].Value[1]
}
```
config/samples/elasticweb_v1_elasticweb.yaml
```bash
apiVersion: elasticweb.example.com/v1
kind: ElasticWeb
metadata:
  name: elasticweb-sample
spec:
  # TODO(user): Add fields here
  # Add fields here
  name: http
  nodePort: 30004
  port: 8081
  podPort: 8080
  image: hengyunabc/expose-prometheus-demo:0.0.1-SNAPSHOT
  targetPort: 8080
  mtargetPort: 8090
  singlePodQPS: 50
  totalQPS: 10
  resources:
    limits:
      cpu: 800m
      memory: 500Mi
    requests:
      cpu: 100m
      memory: 100Mi
  protocol: TCP
```

ServiceMonitor.yaml
```bash
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: elasticweb-sample-service-monitor
  labels:
    app: elasticweb-sample
    release: prometheus
spec:
  selector:
    matchLabels:
      app: elasticweb-sample
  endpoints:
  - port: metric-traffic
    path: "/actuator/prometheus"
```
![image](https://user-images.githubusercontent.com/35655760/156906893-ce5ff18c-f8ea-4842-9c43-ecbb5071e5fa.png)

http://devgou.com/article/Prometheus/   【指标含义如上】
http_server_requests_seconds_count：当前系统接收到的HTTP请求总量
![image](https://user-images.githubusercontent.com/35655760/156906907-ef5e3ed3-bb89-4e2f-bba8-5d52aadb0ff4.png)
![image](https://user-images.githubusercontent.com/35655760/156906914-cb4b35c0-72e8-4c55-ac99-34291849f5e2.png)


请求方式
![image](https://user-images.githubusercontent.com/35655760/156906919-11a9e5e7-4e72-454c-a372-3a9c093b8836.png)


参数调整：
go run main.go -c 4 -n 1000 -u localhost:8080
【如下，30s更新一次。】可以看到平均pqs的变化过程
会使用go执行系统命令来更新config/samples/elasticweb_v1_elasticweb.yaml文件。然后进行kubectl apply
【同时，结合50/55.可以将单机的QPS设置为50.这样方便验证】
![image](https://user-images.githubusercontent.com/35655760/156906921-91b093b1-3fa9-4155-bb0c-8686d27d0f54.png)

接口方式
![image](https://user-images.githubusercontent.com/35655760/156906924-114d076b-32da-468f-9df6-0188c55c4612.png)

自动扩展：
![image](https://user-images.githubusercontent.com/35655760/156906927-b6fe1fa3-3b62-437c-a7b9-b2e79b4b2465.png)


自动收缩
![image](https://user-images.githubusercontent.com/35655760/156906930-297fe644-8f6c-439d-bfdf-e804f12ee7b4.png)



### 6.5 压测工具
https://segmentfault.com/a/1190000020211494

![image](https://user-images.githubusercontent.com/35655760/156906935-cb265f94-2f1e-490a-8247-af4294572d4a.png)




## 7. 常用命令
```bash
//使用operator-sdk创建operator app
operator-sdk init --domain=example.com --repo=github.com/elasticweb

//使用operator-sdk创建operator api
operator-sdk create api --group elasticweb --version v1 --kind ElasticWeb --resource=true --controller=true

//创建webhook
operator-sdk create webhook --group ship --version v1beta1 --kind Frigate --defaulting --programmatic-validation

//推送镜像前需要登陆
docker login

//构建镜像，并推送到docker hub
make docker-build docker-push IMG=dockerhubyu/elasticweb:010

//启动指定的control镜像
make deploy IMG=dockerhubyu/elasticweb:013

//查看所有名字空间下的pod
kubectl get po --all-namespaces

//查看指定命名空间下的所有资源服务
kubectl get all -n dev

//查看指定命名空间、指定service的信息
kubectl get svc elasticweb-sample -n dev

//查看指定命名空间下的serviceAccount
kubectl get sa -n dev -o yaml

//删除自定义的资源文件【会删除对应的资源】
kubectl delete -f config/samples/elasticweb_v1_elasticweb.yaml

//删除指定名字空间下、指定的pod
kubectl delete po elasticweb-sample-6fbb4f7b4c-prlrv -n dev

//查看指定pod、指定容器、指定namespace的日志
kubectl logs -f elasticweb-controller-manager-5446fbccdb-78f44 -c manager -n elasticweb-system

minikube stop

minikube start --kubernetes-version=v1.20.3



kubectl create ns m11          //创建名字空间

helm install prometheus-operator stable/prometheus-operator -n m11     //helm部署prometheus-operator

kubectl get all -n m11         //查看指定空间下的所有资源

helm upgrade prometheus-operator stable/prometheus-operator --values=values_servicemonitor.yml -n m11

kubectl port-forward -n m11 prometheus-prometheus-operator-prometheus-0 9090

kubectl port-forward -n m11 alertmanager-prometheus-operator-alertmanager-0 9093
```
## 8. 问题整理及其解决
### 8.1 failed to initialize project: unable to scaffold with "base.go.kubebuilder.io/v3": exit status 1
![image](https://user-images.githubusercontent.com/35655760/156906956-b4776e9b-60ee-42a6-80c9-a1856c400157.png)

解决：
### 8.2 imports k8s.io/api/core/v1 from implicitly required module;
![image](https://user-images.githubusercontent.com/35655760/156906962-b0b71c8a-24f2-4497-91cc-ce8e18ce87d9.png)

https://www.cnblogs.com/taoshihan/p/15333562.html
### 8.3 provided port is already allocated
![image](https://user-images.githubusercontent.com/35655760/156906966-0e89f6f1-bafb-4570-8b8f-3917e04ba86a.png)

解决：
![image](https://user-images.githubusercontent.com/35655760/156906972-50425a06-e29e-4a58-b415-e792d6353ef7.png)
![image](https://user-images.githubusercontent.com/35655760/156906974-69a39285-e307-4493-a86b-d62b2c833cfc.png)

https://blog.csdn.net/weixin_48140874/article/details/111058217

### 8.4 Protocol 类型问题
![image](https://user-images.githubusercontent.com/35655760/156906976-bdc9eae0-ea2e-4039-96d7-a87fcee59930.png)

解决：找到源码，查看-》调整
### 8.5 sf.IsExported undefined
![image](https://user-images.githubusercontent.com/35655760/156906980-1a56b2fe-7611-4aec-9c4b-079d755792de.png)

解决：Dockerfile golang的版本问题【本质问题是：本地用的是go1.17.1编写/运行    而dockerfile中是1.16】

### 8.6 Failed to execute goal org.apache.maven.plugins:maven-surefire-plugin:2.12.4:test (default-test) on project fakesmtp: There are test failures
————————————————
解决：https://blog.csdn.net/weixin_35821291/article/details/122885975

## 9. 参考资料

k8s入门可以查看：
https://www.bilibili.com/video/BV1cK411p7MY?p=10&spm_id_from=333.1007.top_right_bar_window_history.content.click

官网资料：
https://kubernetes.io/zh/docs/reference/

operator：
https://www.shangmayuan.com/a/d50647bb38034f6cab304bce.html
https://www.cnblogs.com/leffss/p/14732645.html  【operator-sdk实战】
https://xinchen.blog.csdn.net/article/details/113822065 【operator kubebuilder实战】

整理的资料：
https://blog.csdn.net/weixin_35821291/article/details/122700497?spm=1001.2014.3001.5501
https://blog.csdn.net/weixin_35821291/article/details/122725378?spm=1001.2014.3001.5501
https://blog.csdn.net/weixin_35821291/article/details/122738531?spm=1001.2014.3001.5501


