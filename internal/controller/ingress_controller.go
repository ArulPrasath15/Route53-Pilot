package ingressController

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/route53"
)

// IngressReconciler reconciles an Ingress object
type IngressReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=networking.k8s.io.route53pilot,resources=ingresses/finalizers,verbs=update

// TODO: Check if the last-applied-configuration annotation is present and if it matches the current configuration
// else update the DNS record

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		log.Info("Ingress deleted")
		return ctrl.Result{}, nil
	}

	activate := ingress.Annotations["route53.kubernetes.io/activate"]
	if activate != "true" {
		return ctrl.Result{}, nil
	}

	last_applied_configuration := ingress.Annotations["route53pilot/last-applied-configuration"]
	data := make(map[string]interface{})

	// Unmarshal the JSON string into the map
	err := json.Unmarshal([]byte(last_applied_configuration), &data)
	if err != nil {
		log.Info("Failed to unmarshal last-applied-configuration")
		return ctrl.Result{}, nil
	}
	last_applied_configuration_DomainName := data["domain-name"]
	last_applied_configuration_SubDomainName := data["subdomain-name"]
	last_applied_configuration_Region := data["region"]

	// Check if the last-applied-configuration annotation is present and if it matches the current configuration
	if last_applied_configuration_DomainName == ingress.Annotations["route53.kubernetes.io/domain-name"] &&
		last_applied_configuration_SubDomainName == ingress.Annotations["route53.kubernetes.io/subdomain-name"] &&
		last_applied_configuration_Region == ingress.Annotations["route53.kubernetes.io/region"] {
		log.Info("Last-applied-configuration matches the current configuration")
		return ctrl.Result{}, nil
	}

	// log.Info("Ingress identified for Route 53 management " + ingress.Name)

	// // Check if the ingress is being deleted and if the finalizer is not present
	// if !ingress.ObjectMeta.DeletionTimestamp.IsZero() && !containsString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer") {
	// 	log.Info("Ingress deleting")
	// 	return ctrl.Result{}, nil
	// }

	// Fetch domain name from annotation
	domainName := ingress.Annotations["route53.kubernetes.io/domain-name"]
	if domainName == "" {
		log.Info("Domain name annotation missing")
		return ctrl.Result{}, nil
	}
	if !isValidDNSName("DOMAIN", domainName) {
		log.Info("Invalid domain name")
		return ctrl.Result{}, nil
	}

	// Fetch subdomain from annotation
	subDomainName := ingress.Annotations["route53.kubernetes.io/subdomain-name"]
	if subDomainName == "" {
		log.Info("Subdomain name annotation missing")
		return ctrl.Result{}, nil
	}
	if !isValidDNSName("SUBDOMAIN", subDomainName) {
		log.Info("Invalid subdomain name")
		return ctrl.Result{}, nil
	}

	// Fetch region from annotation or default to us-east-1
	region := ingress.Annotations["route53.kubernetes.io/region"]
	if region == "" {
		region = "us-east-1"
	}

	// Create a new AWS session
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region),
	})
	if err != nil {
		log.Info("Failed to create AWS session")
		return ctrl.Result{}, nil
	}

	// Create a new Route 53 client
	r53 := route53.New(sess)

	// Retrieve the hosted zone ID using the domain name
	hostedZoneID, err := r.findHostedZoneID(r53, domainName)
	if err != nil {
		log.Info(err.Error())
		log.Info(fmt.Sprintf("Failed to find hosted zone ID for %s", domainName))
		return ctrl.Result{}, nil
	}

	// Get the LoadBalancer IP from the Ingress
	lbIP, err := r.getLoadBalancerIP(ctx, &ingress)
	if err != nil {
		log.Info("Waiting to get loadBalancer IP")
		return ctrl.Result{}, nil
	}

	action := "UPSERT"
	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Ingress is being deleted")
		action = "DELETE"
	}

	recordName := subDomainName + "." + domainName

	// Create or update the DNS record
	if err := r.createOrUpdateDNSRecord(r53, hostedZoneID, recordName, lbIP, action); err != nil {
		log.Info("Failed to manage Route 53 record")
		return ctrl.Result{}, nil
	}

	// Add finalizer to the Ingress
	if ingress.ObjectMeta.DeletionTimestamp.IsZero() && !containsString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer") {
		ingress.ObjectMeta.Finalizers = append(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Remove finalizer from the Ingress for Deletion
	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		ingress.ObjectMeta.Finalizers = removeString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, nil
		}
		log.Info("DNS Record deleted successfully")
		return ctrl.Result{}, nil
	}

	// Update the last-applied-configuration annotation
	if action == "UPSERT" {
		annotationMap := map[string]interface{}{
			"domain-name":    domainName,
			"subdomain-name": subDomainName,
			"region":         region,
		}
		annotationValue, err := json.Marshal(annotationMap)
		if err != nil {
			panic(err)
		}

		currentAnnotations := ingress.ObjectMeta.Annotations
		if currentAnnotations == nil {
			currentAnnotations = make(map[string]string)
		}

		currentAnnotations["route53pilot/last-applied-configuration"] = string(annotationValue)
		ingress.ObjectMeta.Annotations = currentAnnotations

		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, nil
		}

		log.Info("Successfully created DNS entry")
		return ctrl.Result{}, nil
	}
	return ctrl.Result{}, nil
}

func (r *IngressReconciler) findHostedZoneID(r53 *route53.Route53, domainName string) (string, error) {
	input := &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(domainName),
	}
	result, err := r53.ListHostedZonesByName(input)
	if err != nil {
		return "", err
	}
	for _, zone := range result.HostedZones {
		if *zone.Name == domainName+"." {
			return *zone.Id, nil
		}
	}
	return "", fmt.Errorf("no hosted zone found matching domain name: %s", domainName)
}

func (r *IngressReconciler) getLoadBalancerIP(ctx context.Context, ingress *networkingv1.Ingress) (string, error) {
	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		return "", fmt.Errorf("waiting to get LoadBalancer IP for Ingress")
	}
	hostname := ingress.Status.LoadBalancer.Ingress[0].Hostname
	return hostname, nil
}

func (r *IngressReconciler) createOrUpdateDNSRecord(r53 *route53.Route53, zoneID, recordName, recordValue, action string) error {
	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(zoneID),
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(action),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name: aws.String(recordName),
						Type: aws.String("CNAME"),
						TTL:  aws.Int64(300),
						ResourceRecords: []*route53.ResourceRecord{
							{
								Value: aws.String(recordValue),
							},
						},
					},
				},
			},
		},
	}
	_, err := r53.ChangeResourceRecordSets(input)
	return err
}

func (r *IngressReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Complete(r)
}

func containsString(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	for i, v := range slice {
		if v == s {
			return append(slice[:i], slice[i+1:]...)
		}
	}
	return slice
}

func isValidDNSName(domainType string, name string) bool {
	if len(name) > 255 {
		return false
	}
	// Pattern to match each part of DNS (subdomains + top-level domain)
	if domainType == "SUBDOMAIN" {
		dnsPattern := `^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`
		matched, _ := regexp.MatchString(dnsPattern, name)
		return matched
	}
	dnsPattern := `^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`
	matched, _ := regexp.MatchString(dnsPattern, name)
	return matched
}
