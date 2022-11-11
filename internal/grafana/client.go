package grafana

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"crypto/tls"

	"github.com/sirupsen/logrus"
)

// Client implements a Grafana API client, and contains the instance's base URL
// and API key, along with an HTTP client used to request the API.
// use either APIKey or Username/Password
type Client struct {
	BaseURL    string
	APIKey     string
	Username   string
	Password   string
	SkipVerify bool
	httpClient *http.Client
}

// NewClient returns a new Grafana API client from a given base URL and API key.
func NewClient(baseURL string, apiKey string, username string, password string, SkipVerify bool) (c *Client) {
	// Grafana doesn't support double slashes in the API routes, so we strip the
	// last slash if there's one, because request() will append one anyway.
	if strings.HasSuffix(baseURL, "/") {
		baseURL = baseURL[:len(baseURL)-1]
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: SkipVerify},
	}

	return &Client{
		BaseURL:    baseURL,
		APIKey:     apiKey,
		Username:   username,
		Password:   password,
		httpClient: &http.Client{Transport: tr},
	}
}

// request preforms an HTTP request on a given endpoint, with a given method and
// body. The endpoint is the Grafana API route to request, without the "/api/"
// part. If the request doesn't require a body, the function has to be called
// with "nil" as the "body" parameter.
// Returns the response body (as a []byte containing JSON data).
// Returns an error if there was an issue initialising the request, performing
// it or reading the response body. Also returns an error on non-200 response
// status codes. If the status code is 404, a standard error is returned, if the
// status code is neither 200 nor 404 an error of type httpUnkownError is
// returned.
func (c *Client) request(method string, endpoint string, body []byte) ([]byte, error) {
	route := "/api/" + endpoint

	logrus.WithFields(logrus.Fields{
		"route":  route,
		"method": method,
	}).Debug("Querying the Grafana HTTP API")

	url := c.BaseURL + route

	// Create the request
	req, err := http.NewRequest(method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}

	// Add the API key to the request as an Authorization HTTP header
	if c.APIKey != "" {
		authHeader := fmt.Sprintf("Bearer %s", c.APIKey)
		req.Header.Add("Authorization", authHeader)
	} else {
		req.SetBasicAuth(c.Username, c.Password)
	}

	// If the request isn't a GET, the body will be sent as JSON, so we need to
	// append the appropriate header
	if method != "GET" {
		req.Header.Add("Content-Type", "application/json")
	}

	// Perform the request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	logrus.WithFields(logrus.Fields{
		"route":  route,
		"method": method,
		"code":   resp.StatusCode,
	}).Info("Grafana API response")

	// Read the response body
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Return an error if the Grafana API responded with a non-200 status code.
	// We perform this here because http.Client.Do() doesn't return with an
	// error on non-200 status codes.
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusNotFound {
			err = fmt.Errorf("%s not found (404)", url)
		} else {
			// Return an httpUnkownError error if the status code is neither 200
			// nor 404
			err = newHttpUnknownError(resp.StatusCode)
		}
	}

	// Return the response body along with the error. This allows callers to
	// process httpUnkownError errors by displaying an error message located in
	// the response body along with the data contained in the error.
	return respBody, err
}

// httpUnkownError represents an HTTP error, created from an HTTP response where
// the status code is neither 200 nor 404.
type httpUnkownError struct {
	StatusCode int
}

// newHttpUnknownError creates and returns a new httpUnkownError error using
// the provided status code.
func newHttpUnknownError(statusCode int) *httpUnkownError {
	return &httpUnkownError{
		StatusCode: statusCode,
	}
}

// Error implements error.Error().
func (e *httpUnkownError) Error() string {
	return fmt.Sprintf("Unknown HTTP error: %d", e.StatusCode)
}
