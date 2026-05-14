#!/usr/bin/env bash
#
# request-vpc-peering-quota-increase.sh — file an AWS Support case to raise
# the per-VPC peering-connections quota from the default 50 to 125 (the
# AWS service maximum). Per RESEARCH §Open Question #3 + D-0.5.06.
#
# Why: Pool A capacity ceiling is 50 tenants per VPC; the default peering
# quota matches that ceiling, so as we approach 50-tenant onboarding we'd
# hit the quota wall. The increase is cheap insurance — file it once,
# AWS approves within a few business days, future-proof until Phase 1
# multi-region.
#
# Idempotency: the script greps existing Support cases for one with the
# same subject before filing. Safe to re-run.
#
# Required: AWS CLI v2; an IAM role with `support:CreateCase` permission
# (a Business or Enterprise Support plan is required for the AWS Support
# API). For Phase 0.5 pilot we may file manually via the Support Center
# Console if Support API access isn't provisioned; the script then
# prints the manual URL.
#
# Threat: T-0.5-vpc-peer-quota-exhaust (DoS at the 50-tenant ceiling).

set -euo pipefail

SUBJECT="Phase 0.5 VPC peering quota increase 50→125 (Neksur SaaS pilot)"
SERVICE_CODE="vpc-peering"
CATEGORY_CODE="vpc-peering-active-peering-connections-per-vpc"
SEVERITY_CODE="low"
COMMUNICATION_BODY=$(cat <<'EOF'
Hello,

Neksur is filing a quota-increase request for active VPC peering connections
per VPC in the us-east-1 region of account 964775859511.

Current default:        50
Requested:              125 (the published service maximum)
Justification:          SaaS pilot architecture connects each customer's
                        VPC to the Neksur VPC via a dedicated peering
                        connection. We are approaching 50 customers in
                        the pilot phase and need headroom for future
                        growth before AWS-side throttling becomes a
                        production-availability concern.

References:
- AWS docs: https://docs.aws.amazon.com/vpc/latest/peering/vpc-peering-connection-quotas.html
- Quota name: "Active VPC peerings per VPC" (service "VPC")

Please raise to 125 at your earliest convenience.

Thanks,
Neksur Operations
EOF
)

# 1. Idempotency check — bail if an open case with the same subject exists.
if aws support describe-cases --output json --no-cli-pager 2>/dev/null \
    | grep -F "$SUBJECT" >/dev/null 2>&1; then
    echo "An open Support case with the same subject already exists; not re-filing."
    exit 0
fi

# 2. Attempt to create the case via the Support API. If the API is not
#    accessible (no Business/Enterprise Support plan), fall back to
#    printing the Support Console URL.
if ! aws support create-case \
    --subject "$SUBJECT" \
    --service-code "$SERVICE_CODE" \
    --severity-code "$SEVERITY_CODE" \
    --category-code "$CATEGORY_CODE" \
    --communication-body "$COMMUNICATION_BODY" \
    --output json --no-cli-pager 2>/tmp/support-create-case.err; then

    echo "AWS Support API call failed (commonly: no Business/Enterprise Support plan, or insufficient IAM permissions)."
    echo "stderr:"
    cat /tmp/support-create-case.err
    echo
    echo "MANUAL FALLBACK — file the case via the AWS Console:"
    echo "  https://us-east-1.console.aws.amazon.com/support/home?region=us-east-1#/case/create?issueType=service-limit-increase&limitType=service-code-vpc&serviceLimitIncreaseType=vpc-peering"
    echo
    echo "Subject: $SUBJECT"
    echo
    exit 1
fi

echo "OK: Support case filed."
