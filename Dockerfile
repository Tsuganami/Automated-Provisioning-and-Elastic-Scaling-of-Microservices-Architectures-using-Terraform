FROM hashicorp/terraform:latest

WORKDIR /app

RUN apk add --no-cache \
    go musl-dev gcc

COPY *.go ./
COPY *.tf ./
COPY ./templates ./templates

# Run without -y so the web-based startup configuration page is shown
# at http://localhost:9090. The user submits the form there before
# terraform begins provisioning.
ENTRYPOINT ["go", "run", "deploy.go"]


