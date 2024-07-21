/*
Copyright 2024.

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
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kongv1 "github.com/debu99/kong-operator/api/v1"
)

// KongServiceReconciler reconciles a KongService object
type KongServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=kong.example.com,resources=kongservices/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the KongService object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.16.3/pkg/reconcile
func (r *KongServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger = log.FromContext(ctx)

	// Fetch the KongService instance
	var kongService kongv1.KongService
	if err := r.Get(ctx, req.NamespacedName, &kongService); err != nil {
		logger.Error(err, "Unable to fetch KongService")
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Log the reconciliation
	logger.Info("Reconciling KongService", "name", kongService.Name)

	// Create the service
	serviceID, err := r.createService(kongService.Spec.Name, kongService.Spec.URL)
	if err != nil {
		logger.Error(err, "Failed to create Kong service")
		return ctrl.Result{}, err
	}

	// Update the status
	kongService.Status.ServiceID = serviceID
	if err := r.Status().Update(ctx, &kongService); err != nil {
		logger.Error(err, "Failed to update KongService status")
		return ctrl.Result{}, err
	}

	logger.Info("Successfully reconciled KongService", "name", kongService.Name, "serviceID", serviceID)

	return ctrl.Result{}, nil
}

func (r *KongServiceReconciler) createService(name, url string) (string, error) {
	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/services", cpUUID)
	payload := strings.NewReader(fmt.Sprintf(`{"name":"%s","url":"%s"}`, name, url))

	req, err := http.NewRequest("POST", apiURL, payload)
	if err != nil {
		return "", err
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("failed to create service, status code: %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}

	return result["id"].(string), nil
}


// SetupWithManager sets up the controller with the Manager.
func (r *KongServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kongv1.KongService{}).
		Complete(r)
}
