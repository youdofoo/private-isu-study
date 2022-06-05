.PHONY: install-alp
install-alp:
	wget https://github.com/tkuchiki/alp/releases/download/v1.0.8/alp_linux_amd64.zip \
	&& sudo unzip alp_linux_amd64.zip -d /usr/local/bin \
	&& sudo chmod 755 /usr/local/bin \
	&& rm alp_linux_amd64.zip


NGINX_LOG=/var/log/nginx/access.log
MATCHING="/api/isu/[^/]+/icon","/assets/.+\.(js|svg|css)","/api/isu/[^/]+/graph","/api/isu/[^/]+","/api/condition/[^/]+","/isu/[^/]+/condition","/isu/[^/]+/graph","/isu/[^/]+"
FIELDS=count,2xx,3xx,5xx,method,uri,min,max,sum,avg,p99

.PHONY: alp
alp:
	sudo alp ltsv --file ${NGINX_LOG} -m ${MATCHING} -o ${FIELDS} --sort sum --reverse


SQ_LOG=/var/log/mysql/mysql-slow.log
.PHONY: pt
pt:
	sudo pt-query-digest ${SQ_LOG}

.PHONY: slow
slow:
	sudo mysqldumpslow -s t | head -n 20

.PHONY: deploy-app
deploy-app:
	cd webapp/golang && make build && sudo systemctl restart isu-go.service

.PHONY: reset-log
reset-log:
	sudo bash -c 'echo "" > /var/log/nginx/access.log'
	sudo bash -c 'echo "" > /var/log/mysql/mysql-slow.log'

.PHONY: pprof
pprof:
	go tool pprof -http=localhost:9999 http://localhost:6060/debug/pprof/profile

.PHONY: deploy-nginx
deploy-nginx:
	sudo cp -rf nginx/* /etc/nginx/ && sudo systemctl restart nginx

.PHONY: deploy-mysql
deploy-mysql:
	sudo cp -rf mysql/mysqld.conf /etc/mysql/mysql.conf.d/ && sudo systemctl restart mysql

.PHONY: deploy
deploy: deploy-app deploy-nginx deploy-mysql
 