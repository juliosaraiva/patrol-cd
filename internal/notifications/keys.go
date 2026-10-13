package notifications

// Annotation keys. They are declared here because api/v1alpha1 is owned by another
// component in this phase; api/v1alpha1/keys.go should mirror them (the values are the
// contract, not the Go identifiers).
const (
	// DefaultAnnotationPrefix prefixes subscription annotations:
	//   <prefix>/subscribe.<trigger>.<service>: "recipient1;recipient2"
	//   <prefix>/subscribe.<service>: "recipient"        (default triggers)
	//   <prefix>/subscriptions: YAML list of {trigger: [...], destinations: [{service, recipients}]}
	DefaultAnnotationPrefix = "notifications.cd.patrol.example"

	subscribeSegment     = "/subscribe."
	subscriptionsSegment = "/subscriptions"
	notifiedPrefix       = "notified."
	legacyRecipients     = "recipients."
)

// SubscribeAnnotationPrefix returns "<prefix>/subscribe.".
func SubscribeAnnotationPrefix(prefix string) string { return prefix + subscribeSegment }

// SubscriptionsAnnotation returns "<prefix>/subscriptions".
func SubscriptionsAnnotation(prefix string) string { return prefix + subscriptionsSegment }

// LegacyRecipientsAnnotationSuffix returns the suffix of the deprecated project-level
// recipients annotation for a prefix: "[<trigger>.]recipients.<prefix>" (the analog of the
// baseline "[<trigger>.]recipients.argocd-notifications.argoproj.io").
func LegacyRecipientsAnnotationSuffix(prefix string) string { return legacyRecipients + prefix }

// DeliveryStateAnnotation returns the delivery-state annotation key for a prefix:
// "notified.<prefix>" (for the default prefix: "notified.notifications.cd.patrol.example").
func DeliveryStateAnnotation(prefix string) string { return notifiedPrefix + prefix }
