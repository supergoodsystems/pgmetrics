# Start from the official Go image
FROM golang:1.17 AS build-env

# Set a working directory
WORKDIR /app

# Clone a GoLang GitHub repository
RUN git clone https://github.com/supergoodsystems/pgmetrics.git .

RUN pwd

RUN go build ./pgmetrics/cmd/pgmetrics/main.go

FROM ubuntu:20.04

WORKDIR /app

# Copy the built binary from the previous stage
COPY --from=build-env /app/main .

# Install curl and other required tools
RUN apt-get update && apt-get install -y curl tar

RUN curl -O -L -o pgdash.tar.gz "https://github.com/rapidloop/pgdash/releases/download/v1.11.0/pgdash_1.11.0_linux_amd64.tar.gz" && \
    tar -xzf pgdash.tar.gz && \
    mv pgdash_1.11.0_linux_amd64/pgdash ./pgdash && \
    rm -rf pgdash_1.11.0_linux_amd64 pgdash.tar.gz

ENTRYPOINT [ "./main", "-f", "json", "|", "/var/data/pgdash_1.11.0_linux_amd64/pgdash", "-a", "$PGDASH_API_KEY", "report", "neondb" ]

