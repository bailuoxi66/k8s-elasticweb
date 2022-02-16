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
