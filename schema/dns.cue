package schema

#DNSRecord: {
	type:      "A" | "AAAA" | "CNAME" | "MX" | "NS" | "SRV" | "TXT"
	name:      string
	content:   string
	ttl:       int & >0 | *600
	priority?: int
}
