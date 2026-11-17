package main

import "strings"

// T63：组件名（含常见别名，已按 componentRepoKey 归一）→ 候选开源仓库 owner/repo。
// 只做候选提示：写 url_components.candidate_repo / asset_components.candidate_repo，
// 不写 components.github_repo（那是确认仓库），也不在扫描时联网校验。
var componentRepos = map[string]string{
	"apache superset":    "apache/superset",
	"superset":           "apache/superset",
	"jenkins":            "jenkinsci/jenkins",
	"wordpress":          "WordPress/WordPress",
	"discuz":             "Discuz/DiscuzX",
	"discuz x":           "Discuz/DiscuzX",
	"gitlab":             "gitlabhq/gitlabhq",
	"gitea":              "go-gitea/gitea",
	"nginx":              "nginx/nginx",
	"apache http server": "apache/httpd",
	"httpd":              "apache/httpd",
	"php":                "php/php-src",
	"mysql":              "mysql/mysql-server",
	"redis":              "redis/redis",
	"mongodb":            "mongodb/mongo",
	"mongo":              "mongodb/mongo",
	"elasticsearch":      "elastic/elasticsearch",
	"elastic search":     "elastic/elasticsearch",
	"kafka":              "apache/kafka",
	"apache kafka":       "apache/kafka",
	"rabbitmq":           "rabbitmq/rabbitmq-server",
	"rabbit mq":          "rabbitmq/rabbitmq-server",
	"memcached":          "memcached/memcached",
	"nacos":              "alibaba/nacos",
	"etcd":               "etcd-io/etcd",
	"tomcat":             "apache/tomcat",
	"apache tomcat":      "apache/tomcat",
	"spring boot":        "spring-projects/spring-boot",
	"springboot":         "spring-projects/spring-boot",
}

// componentRepoKey 归一组件名：小写、去掉「Discuz! X」这类感叹号、压缩空白。
func componentRepoKey(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.NewReplacer("!", "", "！", "").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// componentCandidateRepo 返回组件名的候选仓库；映射表里没有则返回空串（不猜）。
func componentCandidateRepo(name string) string {
	return componentRepos[componentRepoKey(name)]
}
