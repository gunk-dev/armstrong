package schema

#DNSRecord: {
	type:     "A" | "CNAME" | "TXT" | "MX" | "AAAA"
	name:     string
	content:  string
	ttl:      int | *600
	priority: int | *null
}
