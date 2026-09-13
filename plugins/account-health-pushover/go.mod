module github.com/NoorChasib/cpa-plugins/plugins/account-health-pushover

go 1.26.0

require gopkg.in/yaml.v3 v3.0.1

require github.com/NoorChasib/cpa-plugins/plugins/quota-cache v0.0.0

replace github.com/NoorChasib/cpa-plugins/plugins/quota-cache => ../quota-cache
