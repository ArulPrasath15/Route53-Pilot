package ingressController

import (
	"context"
	"fmt"

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

func (r *IngressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	var ingress networkingv1.Ingress
	if err := r.Get(ctx, req.NamespacedName, &ingress); err != nil {
		log.Error(err, "Failed to get Ingress")
		// log.Info(ingress.GetDeletionTimestamp().GoString())
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Check if the Ingress is being deleted
	if !ingress.GetDeletionTimestamp().IsZero() {
		// Ingress is in the process of being deleted
		annotations := ingress.GetAnnotations()
		log.Info("Ingress is being deleted", "annotations", annotations)

		// Example: Process or log a specific annotation
		if value, exists := annotations["your-annotation-key"]; exists {
			log.Info("Annotation value", "your-annotation-key", value)
		}
	}
	// TODO Add validation for required annotations
	// TODO Handle wait for hostname

	// Fetch domain name from annotation
	domainName := ingress.Annotations["route53.kubernetes.io/domain-name"]
	if domainName == "" {
		log.Info("Domain name annotation missing")
		return ctrl.Result{}, nil
	}

	// Fetch region from annotation or default to us-east-1
	region := ingress.Annotations["route53.kubernetes.io/region"]
	if region == "" {
		region = "us-east-1"
	}

	// Fetch region from annotation or default to us-east-1
	subDomainName := ingress.Annotations["route53.kubernetes.io/subdomain-name"]
	if subDomainName == "" {
		log.Info("Subdomain name annotation missing")
		return ctrl.Result{}, nil
	}

	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(region),
	})
	if err != nil {
		log.Error(err, "Failed to create AWS session")
		return ctrl.Result{}, err
	}

	r53 := route53.New(sess)

	// Retrieve the hosted zone ID using the domain name
	hostedZoneID, err := r.findHostedZoneID(r53, domainName)
	if err != nil {
		log.Error(err, "Failed to find hosted zone ID")
		return ctrl.Result{}, err
	}

	lbIP, err := r.getLoadBalancerIP(ctx, &ingress)
	if err != nil {
		log.Error(err, "Failed to get LoadBalancer IP")
		return ctrl.Result{}, err
	}

	action := "UPSERT"
	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info("Ingress is being deleted")
		action = "DELETE"
	}

	if ingress.ObjectMeta.DeletionTimestamp.IsZero() && !containsString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer") {
		ingress.ObjectMeta.Finalizers = append(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, err
		}
	}

	recordName := subDomainName + "." + domainName

	if err := r.createOrUpdateDNSRecord(r53, hostedZoneID, recordName, lbIP, action); err != nil {
		log.Error(err, "Failed to manage Route 53 record")
		return ctrl.Result{}, err
	}

	if !ingress.ObjectMeta.DeletionTimestamp.IsZero() {
		ingress.ObjectMeta.Finalizers = removeString(ingress.ObjectMeta.Finalizers, "route53pilot/finalizer")
		if err := r.Update(ctx, &ingress); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	log.Info("Successfully managed Route 53 record")
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
		if *zone.Name == domainName+"." { // Ensure exact match
			return *zone.Id, nil
		}
	}
	return "", fmt.Errorf("no hosted zone found matching domain name: %s", domainName)
}

func (r *IngressReconciler) getLoadBalancerIP(ctx context.Context, ingress *networkingv1.Ingress) (string, error) {
	if len(ingress.Status.LoadBalancer.Ingress) == 0 {
		return "", fmt.Errorf("no LoadBalancer IP found for Ingress")
	}
	ip := ingress.Status.LoadBalancer.Ingress[0].IP
	var hostname string
	if ip == "" {
		// Fallback to hostname resolution if IP is not directly provided
		hostname = ingress.Status.LoadBalancer.Ingress[0].Hostname
	}
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
