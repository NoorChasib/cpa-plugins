module github.com/NoorChasib/cpa-plugins/plugins/quota-glance

go 1.26.0

replace github.com/NoorChasib/cpa-plugins/plugins/quota-cache => ../quota-cache

require (
	github.com/NoorChasib/cpa-plugins/plugins/quota-cache v0.0.0-00010101000000-000000000000
	github.com/fsnotify/fsnotify v1.9.0
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sys v0.29.0 // indirect
