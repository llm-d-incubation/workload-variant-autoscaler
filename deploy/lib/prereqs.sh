#!/usr/bin/env bash
#
# Prerequisite checks and interactive prompts for deploy/install.sh.
# Requires vars: REQUIRED_TOOLS, E2E_TESTS_ENABLED, ENVIRONMENT, GATEWAY_PROVIDER.
# Requires funcs: log_info/log_warning/log_success.
# Sets/exports: INSTALL_CLIENT_TOOLS, INSTALL_GATEWAY_CTRLPLANE.
#

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=()

    # Check for required tools
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=($tool)
        fi
    done

    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_warning "Missing required tools: ${missing_tools[*]}"
        if [ "$E2E_TESTS_ENABLED" == "false" ]; then
            prompt_install_missing_tools
        else
            log_info "E2E tests enabled - will install missing tools by default"
        fi
    fi

    log_success "All generic prerequisites tools met"
}

prompt_install_missing_tools() {
    echo ""
    log_info "The client tools are required to install and manage the infrastructure."
    echo "  You can either:"
    echo "      1. Install them manually."
    echo "      2. Let the script attempt to install them for you."
    echo "The environment is currently set to: ${ENVIRONMENT}"

    while true; do
        read -p "Do you want to install the required client tools? (y/n): " -r answer
        case $answer in
            [Yy]* )
                INSTALL_CLIENT_TOOLS="true"
                log_success "Will install client tools when deploying llm-d"
                break
                ;;
            [Nn]* )
                INSTALL_CLIENT_TOOLS="false"
                log_warning "Will not install the required client tools. Please install them manually."
                break
                ;;
            * )
                echo "Please answer y (yes) or n (no)."
                ;;
        esac
    done
}

prompt_gateway_installation() {
    echo ""
    log_info "Gateway Control Plane Configuration"
    echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo "The Gateway control plane (${GATEWAY_PROVIDER}) is required to serve requests."
    echo "You can either:"
    echo "  1. Install the Gateway control plane (recommended for new clusters or emulated clusters)"
    echo "  2. Use an existing Gateway control plane in your cluster (recommended for production clusters)"
    echo "The environment is currently set to: ${ENVIRONMENT}"

    while true; do
        read -p "Do you want to install the Gateway control plane? (y/n): " -r answer
        case $answer in
            [Yy]* )
                INSTALL_GATEWAY_CTRLPLANE="true"
                log_success "Will install Gateway control plane ($GATEWAY_PROVIDER) when deploying llm-d"
                break
                ;;
            [Nn]* )
                INSTALL_GATEWAY_CTRLPLANE="false"
                log_info "Will attempt to use existing Gateway control plane when deploying llm-d"
                break
                ;;
            * )
                echo "Please answer y (yes) or n (no)."
                ;;
        esac
    done

    export INSTALL_GATEWAY_CTRLPLANE
    echo ""
}
