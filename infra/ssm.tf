# Session Manager access to the k3s nodes.
#
# The cluster sits in private subnets behind a bastion, reachable only with the
# EC2 key pair. That makes operating it depend on whoever holds a particular
# private key file, which is both a single point of failure and an access path
# with no audit trail.
#
# Session Manager removes the key from the picture: access is IAM, every session
# is logged in CloudTrail, and nothing has to listen on port 22. The SSM agent
# ships with the Ubuntu image already, so this policy is the only missing piece.
#
# AmazonSSMManagedInstanceCore is the AWS-managed policy for exactly this and
# grants nothing beyond the agent's own needs — it cannot read S3 or touch other
# services.
resource "aws_iam_role_policy_attachment" "k3s_nodes_ssm" {
  role       = aws_iam_role.k3s_nodes.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore"
}
