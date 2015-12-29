package jolokia

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"

	"github.com/influxdb/telegraf/plugins"
)

type Server struct {
	Name     string
	Host     string
	Username string
	Password string
	Port     string
}

type Metric struct {
	Name               string
	Jmx                string
	MultipleMBeans     bool
	SeriesNameOverride string
}

type JolokiaClient interface {
	MakeRequest(req *http.Request) (*http.Response, error)
}

type JolokiaClientImpl struct {
	client *http.Client
}

func (c JolokiaClientImpl) MakeRequest(req *http.Request) (*http.Response, error) {
	return c.client.Do(req)
}

type Jolokia struct {
	jClient JolokiaClient
	Context string
	Servers []Server
	Metrics []Metric
}

func (j *Jolokia) SampleConfig() string {
	return `
  # This is the context root used to compose the jolokia url
  context = "/jolokia/read"

  # List of servers exposing jolokia read service
  [[plugins.jolokia.servers]]
    name = "stable"
    host = "192.168.103.2"
    port = "8180"
    # username = "myuser"
    # password = "mypassword"

  # List of metrics collected on above servers
  # Each metric consists in a name, a jmx path and either a pass or drop slice attributes
  # This collect all heap memory usage metrics
  [[plugins.jolokia.metrics]]
    name = "heap_memory_usage"
    jmx  = "/java.lang:type=Memory/HeapMemoryUsage"
`
// TODO: add new pieces of example config
}

func (j *Jolokia) Description() string {
	return "Read JMX metrics through Jolokia"
}

func (j *Jolokia) getAttr(requestUrl *url.URL) (map[string]interface{}, error) {
	// Create + send request
	req, err := http.NewRequest("GET", requestUrl.String(), nil)
	if err != nil {
		return nil, err
	}
	// TODO: why was this added in prev commit
	//defer req.Body.Close()

	resp, err := j.jClient.MakeRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Process response
	if resp.StatusCode != http.StatusOK {
		err = fmt.Errorf("Response from url \"%s\" has status code %d (%s), expected %d (%s)",
			requestUrl,
			resp.StatusCode,
			http.StatusText(resp.StatusCode),
			http.StatusOK,
			http.StatusText(http.StatusOK))
		return nil, err
	}

	// read body
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Unmarshal json
	var jsonOut map[string]interface{}
	if err = json.Unmarshal([]byte(body), &jsonOut); err != nil {
		return nil, errors.New("Error decoding JSON response")
	}

	return jsonOut, nil
}

/* flattens nested maps into a single fields map
 * eg. if we have something like
 *  "value": {
 *    "Usage": {
 *      "committed": 456,
 *      "used": 123
 *    }
 *  }
 * this will return a map like
 * {"Usage_committed": 456, "Usage_used": 123}
 */
func getNestedFields(data interface{}, fieldName string) map[string]interface{} {
	fields := make(map[string]interface{})
	switch t := data.(type) {
	case map[string]interface{}:
		for key, value := range t {
			newFieldName := key
			if len(fieldName) > 0 {
				newFieldName = fieldName + "_" + key
			}
			for new_key, new_value := range getNestedFields(value, newFieldName) {
				fields[new_key] = new_value
			}
		}
	case interface{}:
		if len(fieldName) == 0 {
			fieldName = "value"
		}
		fields[fieldName] = t
	}
	return fields
}

func (j *Jolokia) Gather(acc plugins.Accumulator) error {
	context := j.Context //"/jolokia/read"
	servers := j.Servers
	metrics := j.Metrics
	tags := make(map[string]string)

	for _, server := range servers {
		tags["server"] = server.Name
		tags["port"] = server.Port
		tags["host"] = server.Host
		for _, metric := range metrics {
			seriesName := "jolokia"
			if len(metric.SeriesNameOverride) > 0 {
				seriesName = metric.SeriesNameOverride
			}

			measurement := metric.Name
			jmxPath := metric.Jmx

			// Prepare URL
			requestUrl, err := url.Parse("http://" + server.Host + ":" +
				server.Port + context + jmxPath)
			if err != nil {
				return err
			}
			if server.Username != "" || server.Password != "" {
				requestUrl.User = url.UserPassword(server.Username, server.Password)
			}

			out, _ := j.getAttr(requestUrl)

			if values, ok := out["value"]; ok {
				switch t := values.(type) {
				case map[string]interface{}:
					if metric.MultipleMBeans {
						measurements := make(map[string]map[string]interface{})
						measurementTags := make(map[string]map[string]string)

						// parse out mbean properties as tags and add a meauserment for each mbean
						for beanName, attributes := range t {
							groupName := ""
							measurementName := ""

							beanName = strings.Replace(beanName, " ", "_", -1)
							properties := strings.Split(beanName[1 + strings.Index(beanName, ":"):], ",")
							beanTags := map[string]string {}
							for k, v := range tags {
								beanTags[k] = v
							}
							for _, v := range properties {
								tag := strings.Split(v, "=")
								if tag[0] == "name" {
									measurementName = tag[1]
									continue
								}
								groupName += tag[1]
								beanTags[tag[0]] = tag[1]
							}

							for k, v := range getNestedFields(attributes, measurementName) {
								if _, ok := measurements[groupName]; !ok {
									measurements[groupName] = make(map[string]interface{})
								}
								measurements[groupName][k] = v
							}
							measurementTags[groupName] = beanTags
						}
						for groupName, measurement := range measurements {
							acc.AddFields(seriesName, measurement, measurementTags[groupName])
						}
					} else {
						acc.AddFields(seriesName, getNestedFields(t, measurement), tags)
					}
				case interface{}:
					acc.AddFields(seriesName, getNestedFields(t, measurement), tags)
				}
			} else {
				fmt.Printf("Missing key 'value' in '%s' output response\n",
					requestUrl.String())
			}
		}
	}

	return nil
}

func init() {
	plugins.Add("jolokia", func() plugins.Plugin {
		return &Jolokia{jClient: &JolokiaClientImpl{client: &http.Client{}}}
	})
}
