package mine

import (
	"testing"

	"github.com/askrike/desprawl/internal/rules"
)

func TestFromHCL(t *testing.T) {
	src := []byte(`
resource "aws_iam_role_policy" "p" {
  role = aws_iam_role.example.name
  policy = jsonencode({
    Statement = [{
      Resource = "${aws_s3_bucket.b.arn}/*"
      Principal = { AWS = aws_iam_role.other[0].arn }
    }]
  })
}

resource "aws_lambda_function" "f" {
  role        = data.aws_iam_role.r.arn
  description = upper(aws_s3_bucket.b.bucket)
  vpc_config {
    subnet_ids = [aws_subnet.a.id]
  }
  name = var.name
  kms_key_arn = azurerm_kubernetes_cluster.c.identity[0].principal_id
  runtime = aws_lambda_function.sibling.runtime
  handler = jsondecode(base64decode(aws_s3_bucket.b.policy)).Version
  lifecycle {
    ignore_changes = [aws_s3_bucket.b.id]
  }
}
`)
	got := map[rules.Rule]bool{}
	for _, r := range fromHCL(src) {
		got[r] = true
	}
	want := []rules.Rule{
		{Source: "aws_iam_role_policy", Path: "role", Target: "aws_iam_role", Attr: "name", Match: rules.MatchExact},
		{Source: "aws_iam_role_policy", Path: "policy.Statement.Resource", Target: "aws_s3_bucket", Attr: "arn", Match: rules.MatchEmbedded},
		{Source: "aws_iam_role_policy", Path: "policy.Statement.Principal.AWS", Target: "aws_iam_role", Attr: "arn", Match: rules.MatchExact},
		{Source: "aws_lambda_function", Path: "role", Target: "aws_iam_role", Attr: "arn", Match: rules.MatchExact},
		{Source: "aws_lambda_function", Path: "vpc_config.subnet_ids", Target: "aws_subnet", Attr: "id", Match: rules.MatchExact},
		{Source: "aws_lambda_function", Path: "kms_key_arn", Target: "azurerm_kubernetes_cluster", Attr: "identity.0.principal_id", Match: rules.MatchExact},
	}
	for _, w := range want {
		w.Origin = rules.OriginDocs
		if !got[w] {
			t.Errorf("missing %+v", w)
		}
	}
	// var.*, values transformed by functions, and lifecycle blocks are not references.
	if len(got) != len(want) {
		t.Errorf("got %d rules, want %d: %+v", len(got), len(want), got)
	}
}
