# Storefront: a static site behind CloudFront.
resource "aws_s3_bucket" "storefront" {
  bucket = "shop-storefront"
}

resource "aws_cloudfront_origin_access_control" "storefront" {
  name                              = "storefront"
  origin_access_control_origin_type = "s3"
  signing_behavior                  = "always"
  signing_protocol                  = "sigv4"
}

resource "aws_cloudfront_distribution" "storefront" {
  enabled             = true
  default_root_object = "index.html"
  origin {
    domain_name              = aws_s3_bucket.storefront.bucket_regional_domain_name
    origin_id                = "storefront"
    origin_access_control_id = aws_cloudfront_origin_access_control.storefront.id
  }
  default_cache_behavior {
    target_origin_id       = "storefront"
    viewer_protocol_policy = "redirect-to-https"
    allowed_methods        = ["GET", "HEAD"]
    cached_methods         = ["GET", "HEAD"]
    forwarded_values {
      query_string = false
      cookies { forward = "none" }
    }
  }
  restrictions {
    geo_restriction { restriction_type = "none" }
  }
  viewer_certificate { cloudfront_default_certificate = true }
}

# The cluster the API runs on.
resource "aws_iam_role" "cluster" {
  name = "shop-eks"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "eks.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_eks_cluster" "main" {
  name     = "shop"
  role_arn = aws_iam_role.cluster.arn
  vpc_config {
    subnet_ids = ["subnet-0a1b2c3d", "subnet-4e5f6a7b"]
  }
}

# Data.
resource "aws_db_instance" "main" {
  identifier        = "shop"
  engine            = "postgres"
  instance_class    = "db.t4g.micro"
  allocated_storage = 20
  username          = "shop"
  manage_master_user_password = true
}

resource "aws_elasticache_replication_group" "sessions" {
  replication_group_id = "shop-sessions"
  description          = "sessions"
  engine               = "redis"
  node_type            = "cache.t4g.micro"
}

resource "aws_s3_bucket" "uploads" {
  bucket = "shop-uploads"
}

resource "aws_sqs_queue" "orders" {
  name = "shop-orders"
}

# Thumbnails for uploaded product images.
resource "aws_iam_role" "thumbnailer" {
  name = "shop-thumbnailer"
  assume_role_policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Principal = { Service = "lambda.amazonaws.com" }, Action = "sts:AssumeRole" }]
  })
}

resource "aws_iam_role_policy" "thumbnailer" {
  role = aws_iam_role.thumbnailer.id
  policy = jsonencode({
    Version   = "2012-10-17"
    Statement = [{ Effect = "Allow", Action = ["s3:GetObject", "s3:PutObject"], Resource = "${aws_s3_bucket.uploads.arn}/*" }]
  })
}

resource "aws_lambda_function" "thumbnailer" {
  function_name = "shop-thumbnailer"
  role          = aws_iam_role.thumbnailer.arn
  runtime       = "python3.12"
  handler       = "main.handler"
  filename      = "thumbnailer.zip"
}

resource "aws_lambda_permission" "uploads" {
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.thumbnailer.function_name
  principal     = "s3.amazonaws.com"
  source_arn    = aws_s3_bucket.uploads.arn
}

resource "aws_s3_bucket_notification" "uploads" {
  bucket = aws_s3_bucket.uploads.id
  lambda_function {
    lambda_function_arn = aws_lambda_function.thumbnailer.arn
    events              = ["s3:ObjectCreated:*"]
  }
}
