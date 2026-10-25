//go:build !linux

package factory

func readHost(string) hostReading {
	return hostReading{Metrics: hostMetrics{Errors: map[string]string{
		"cpu": "Host metrics require Linux", "load": "Host metrics require Linux",
		"memory": "Host metrics require Linux", "disk": "Host metrics require Linux",
	}}}
}
