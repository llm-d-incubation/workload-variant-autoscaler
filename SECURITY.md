# Security Policy

## Reporting a Vulnerability

The llm-d project takes security seriously. We appreciate your efforts to responsibly disclose your findings.

### Reporting Process

If you discover a security vulnerability in Workload-Variant-Autoscaler, please report it by emailing:

**security@llm-d.ai**

Please include the following information in your report:

- Description of the vulnerability
- Steps to reproduce the issue
- Affected versions
- Potential impact
- Any suggested mitigations or fixes (if available)

### What to Expect

- **Initial Response**: We will acknowledge receipt of your vulnerability report within 3 business days.
- **Status Updates**: We will provide status updates on the remediation process at least every 7 days.
- **Disclosure Timeline**: We aim to resolve critical vulnerabilities within 90 days. We will coordinate with you on public disclosure timing.
- **Credit**: With your permission, we will credit you in the security advisory and release notes.

### Scope

This security policy applies to:

- Workload-Variant-Autoscaler controller code
- Custom Resource Definitions (CRDs)
- Kubernetes RBAC configurations
- Deployment manifests and configurations
- Dependencies and third-party libraries

### Out of Scope

The following are generally considered out of scope:

- Vulnerabilities in third-party dependencies already publicly disclosed
- Issues requiring physical access to infrastructure
- Social engineering attacks
- Denial of service attacks requiring excessive resources

### Security Best Practices

When deploying Workload-Variant-Autoscaler:

1. **RBAC**: Review and apply least-privilege RBAC policies
2. **Network Policies**: Implement Kubernetes NetworkPolicies to restrict traffic
3. **Image Security**: Use signed container images and regularly scan for vulnerabilities
4. **Secrets Management**: Use Kubernetes Secrets or external secret managers for sensitive data
5. **Monitoring**: Enable audit logging and monitor for suspicious activity
6. **Updates**: Keep the controller and dependencies updated with the latest security patches

### Supported Versions

Security updates are provided for:

- The latest stable release
- The previous stable release (for 90 days after new release)

Older versions may receive security updates at the maintainers' discretion for critical vulnerabilities.

### Security Advisories

Security advisories will be published at:

- GitHub Security Advisories: https://github.com/llm-d/llm-d-workload-variant-autoscaler/security/advisories
- Project documentation: https://llm-d.ai/docs/security

### Vulnerability Disclosure Policy

We follow coordinated vulnerability disclosure principles:

1. Researchers report vulnerabilities privately
2. We work with reporters to understand and fix the issue
3. We prepare patches and security advisories
4. We coordinate public disclosure with the reporter
5. We release patches and publish advisories simultaneously

Thank you for helping keep Workload-Variant-Autoscaler and the llm-d ecosystem secure.
