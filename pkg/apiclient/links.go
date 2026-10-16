package apiclient

// LinkInfo is one evaluated deep link (application.links, resource.links,
// project.links in patrol-cm).
type LinkInfo struct {
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Description *string `json:"description,omitempty"`
	IconClass   *string `json:"iconClass,omitempty"`
}

// LinksResponse is the response of the deep link routes
// (GET /api/v1/applications/{name}/links, .../resource/links, /api/v1/projects/{name}/links).
type LinksResponse struct {
	Items []LinkInfo `json:"items"`
}
