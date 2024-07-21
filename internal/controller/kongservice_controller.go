package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

  apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kongv1 "github.com/debu99/kong-operator/api/v1"
)

const kongFinalizerName = "kong.example.com/finalizer"

// KongServiceReconciler reconciles a KongService object
type KongServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *KongServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the KongService instance
	var kongService kongv1.KongService
	err := r.Get(ctx, req.NamespacedName, &kongService)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// The KongService resource has been deleted
			logger.Info("KongService resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		logger.Error(err, "Failed to get KongService")
		return ctrl.Result{}, err
	}
	// Log the reconciliation
	logger.Info("Reconciling KongService", "name", kongService.Name)

	// Check if the KongService instance is marked to be deleted
	if !kongService.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &kongService)
	}

	// Add finalizer if it doesn't exist
	if !controllerutil.ContainsFinalizer(&kongService, kongFinalizerName) {
		controllerutil.AddFinalizer(&kongService, kongFinalizerName)
		if err := r.Update(ctx, &kongService); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Check if the service exists in the status
	if kongService.Status.ServiceID == "" {
		// Service doesn't exist, create it
		serviceID, err := r.createService(ctx, kongService.Spec.Name, kongService.Spec.URL)
		if err != nil {
			logger.Error(err, "Failed to create Kong service")
			return ctrl.Result{}, err
		}
		kongService.Status.ServiceID = serviceID
		logger.Info("Successfully created KongService", "name", kongService.Spec.Name, "serviceID", serviceID)
	} else {
		logger.Info("Kong Service already exists", "name", kongService.Spec.Name, "serviceID", kongService.Status.ServiceID)
	}

	// Initialize RouteIDs map if it's nil
	if kongService.Status.RouteIDs == nil {
		kongService.Status.RouteIDs = make(map[string]string)
	}

	// Create routes if they don't exist
	for _, route := range kongService.Spec.Routes {
		routeName := generateRouteName(kongService.Spec.Name, route.Path)
		if _, exists := kongService.Status.RouteIDs[routeName]; !exists {
			// Route doesn't exist, create it
			routeID, err := r.createRoute(ctx, kongService.Spec.Name, kongService.Status.ServiceID, route)
			if err != nil {
				logger.Error(err, "Failed to create Kong route", "name", routeName)
				return ctrl.Result{}, err
			}
			kongService.Status.RouteIDs[routeName] = routeID
			logger.Info("Successfully created Kong Route", "name", routeName, "routeID", routeID)
		} else {
			logger.Info("Kong Route already exists", "name", routeName, "routeID", kongService.Status.RouteIDs[routeName])
		}
	}

	// Update the status
	if err := r.Status().Update(ctx, &kongService); err != nil {
		logger.Error(err, "Failed to update KongService status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KongServiceReconciler) reconcileDelete(ctx context.Context, kongService *kongv1.KongService) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Delete all routes
	for routeName, routeID := range kongService.Status.RouteIDs {
		if err := r.deleteRoute(ctx, routeID); err != nil {
			logger.Error(err, "Failed to delete Kong route", "name", routeName, "routeID", routeID)
			return ctrl.Result{}, err
		}
		logger.Info("Successfully deleted Kong Route", "name", routeName, "routeID", routeID)
	}

	// Delete the Kong service
	if kongService.Status.ServiceID != "" {
		if err := r.deleteService(ctx, kongService.Status.ServiceID); err != nil {
			logger.Error(err, "Failed to delete Kong service")
			return ctrl.Result{}, err
		}
		logger.Info("Successfully deleted Kong Service", "serviceID", kongService.Status.ServiceID)
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(kongService, kongFinalizerName)
	if err := r.Update(ctx, kongService); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *KongServiceReconciler) createService(ctx context.Context, name, url string) (string, error) {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/services", cpUUID)
	payload := strings.NewReader(fmt.Sprintf(`{"name":"%s","url":"%s"}`, name, url))

	req, err := http.NewRequest("POST", apiURL, payload)
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		logger.Error(fmt.Errorf("API error"), "Failed to create service", "statusCode", resp.StatusCode, "response", string(body))
		return "", fmt.Errorf("failed to create service, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %w", err)
	}

	serviceID, ok := result["id"].(string)
	if !ok {
		return "", fmt.Errorf("service ID not found in response")
	}

	return serviceID, nil
}

func (r *KongServiceReconciler) createRoute(ctx context.Context, serviceName, serviceID string, route kongv1.Route) (string, error) {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return "", fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	routeName := generateRouteName(serviceName, route.Path)

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes", cpUUID)

	routeData := map[string]interface{}{
		"name":    routeName,
		"service": map[string]string{"id": serviceID},
		"paths":   []string{route.Path},
		"tags":    append(route.Tags, serviceName, "k8s-operator"),
	}

	if len(route.Methods) > 0 {
		routeData["methods"] = route.Methods
	}
	if len(route.Hosts) > 0 {
		routeData["hosts"] = route.Hosts
	}
	if route.StripPath != nil {
		routeData["strip_path"] = *route.StripPath
	}
	if route.PreserveHost != nil {
		routeData["preserve_host"] = *route.PreserveHost
	}
	if len(route.Protocols) > 0 {
		routeData["protocols"] = route.Protocols
	} else {
		routeData["protocols"] = []string{"http", "https"}
	}

	payload, err := json.Marshal(routeData)
	if err != nil {
		return "", fmt.Errorf("failed to marshal route data: %w", err)
	}

	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer(payload))
	if err != nil {
		return "", fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)
	req.Header.Add("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		logger.Error(fmt.Errorf("API error"), "Failed to create route", "statusCode", resp.StatusCode, "response", string(body))
		return "", fmt.Errorf("failed to create route, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse JSON response: %w", err)
	}

	routeID, ok := result["id"].(string)
	if !ok {
		return "", fmt.Errorf("route ID not found in response")
	}

	return routeID, nil
}

func (r *KongServiceReconciler) deleteService(ctx context.Context, serviceID string) error {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/services/%s", cpUUID, serviceID)

	req, err := http.NewRequest("DELETE", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		logger.Error(fmt.Errorf("API error"), "Failed to delete service", "statusCode", resp.StatusCode, "response", string(body))
		return fmt.Errorf("failed to delete service, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

func (r *KongServiceReconciler) deleteRoute(ctx context.Context, routeID string) error {
	logger := log.FromContext(ctx)

	cpUUID := os.Getenv("CP_UUID")
	konnectToken := os.Getenv("KONNECT_TOKEN")

	if cpUUID == "" || konnectToken == "" {
		return fmt.Errorf("CP_UUID or KONNECT_TOKEN environment variables are not set")
	}

	apiURL := fmt.Sprintf("https://eu.api.konghq.com/v2/control-planes/%s/core-entities/routes/%s", cpUUID, routeID)

	req, err := http.NewRequest("DELETE", apiURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create HTTP request: %w", err)
	}

	req.Header.Add("Authorization", "Bearer "+konnectToken)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		logger.Error(fmt.Errorf("API error"), "Failed to delete route", "statusCode", resp.StatusCode, "response", string(body))
		return fmt.Errorf("failed to delete route, status code: %d, response: %s", resp.StatusCode, string(body))
	}

	return nil
}

func generateRouteName(serviceName, path string) string {
	cleanPath := strings.Trim(path, "/")
	routeName := fmt.Sprintf("%s-%s", serviceName, cleanPath)
	return strings.ReplaceAll(routeName, "/", "-")
}

// SetupWithManager sets up the controller with the Manager.
func (r *KongServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kongv1.KongService{}).
		Complete(r)
}
