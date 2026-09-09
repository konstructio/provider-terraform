FROM alpine

RUN apk add --no-cache curl unzip bash git

# Terraform version
ARG TERRAFORM_VERSION=1.14.9
ARG TARGETARCH

# --- bake kubectl provider + CLI config ---
ARG KUBECTL_PROVIDER_NAMESPACE=gavinbunney
ARG KUBECTL_PROVIDER_VERSION=1.19.0

RUN mkdir -p /plugins/registry.terraform.io/${KUBECTL_PROVIDER_NAMESPACE}/kubectl \
 && curl -fsSL \
    -o /plugins/registry.terraform.io/${KUBECTL_PROVIDER_NAMESPACE}/kubectl/terraform-provider-kubectl_${KUBECTL_PROVIDER_VERSION}_linux_${TARGETARCH}.zip \
    https://github.com/${KUBECTL_PROVIDER_NAMESPACE}/terraform-provider-kubectl/releases/download/v${KUBECTL_PROVIDER_VERSION}/terraform-provider-kubectl_${KUBECTL_PROVIDER_VERSION}_linux_${TARGETARCH}.zip

COPY <<'EOF' /etc/terraformrc
provider_installation {
  filesystem_mirror {
    path    = "/plugins"
    include = ["registry.terraform.io/gavinbunney/kubectl"]
  }
  direct {
    exclude = ["registry.terraform.io/gavinbunney/kubectl"]
  }
}
EOF

ENV TF_CLI_CONFIG_FILE=/etc/terraformrc

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
