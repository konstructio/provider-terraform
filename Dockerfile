FROM alpine

RUN apk add --no-cache curl unzip bash git

# Terraform version
ARG TERRAFORM_VERSION=1.5.7
ARG TARGETARCH

# Download correct binary depending on architecture
RUN curl -fsSL -o /tmp/terraform.zip \
      https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_${TARGETARCH}.zip \
    && unzip /tmp/terraform.zip -d /usr/local/bin \
    && rm /tmp/terraform.zip

# Put the manager binary in /app
WORKDIR /
COPY ./manager .
RUN chmod +x ./manager
COPY gitconfig /.gitconfig

# Create required writable directories
RUN mkdir -p /tf /tmp /logs && chmod -R 777 /tf /logs /tmp

ENTRYPOINT ["/manager"]
