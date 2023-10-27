curl -O -L https://github.com/rapidloop/pgdash/releases/download/v1.11.0/pgdash_1.11.0_linux_amd64.tar.gz >> /var/data
tar xvf /var/data/pgdash_1.11.0_linux_amd64.tar.gz
go build cmd/pgmetrics/main.go
