package schema

#HttpCheck: {
	grace_period:    string | *"10s"
	interval:        string | *"30s"
	method:          string | *"GET"
	path:            string | *"/"
	timeout:         string | *"5s"
	tls_skip_verify: bool | *false
}

#HttpService: {
	internal_port:        int & >=1 & <=65535
	force_https:          bool | *true
	auto_stop_machines:   bool | "off" | "stop" | *"suspend"
	auto_start_machines:  bool | *true
	min_machines_running: int & >=0 | *0
	checks?: [...#HttpCheck]
}

#FlyApp: {
	app:             string
	primary_region:  string
	custom_domains?: [...string]
	[string]:        _

	http_service?: #HttpService
}
