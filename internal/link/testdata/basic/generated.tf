# __generated__ by Terraform
# Please review these resources and move them into your main configuration files.

# __generated__ by Terraform from "app-lambda"
resource "aws_iam_role" "app_lambda" {
  assume_role_policy = jsonencode({
    Statement = [{
      Action = "sts:AssumeRole"
      Effect = "Allow"
      Principal = {
        Service = "lambda.amazonaws.com"
      }
    }]
    Version = "2012-10-17"
  })
  description = null
  name        = "app-lambda"
  path        = "/"
  tags = {
    Name = "app-lambda"
  }
  tags_all = {
    Name = "app-lambda"
  }
}

resource "aws_iam_role_policy_attachment" "app_lambda_basic" {
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
  role       = "app-lambda"
}

resource "aws_iam_role_policy" "app_lambda_sqs" {
  name = "sqs"
  policy = jsonencode({
    Statement = [{
      Action   = ["sqs:ReceiveMessage", "sqs:DeleteMessage"]
      Effect   = "Allow"
      Resource = "arn:aws:sqs:eu-central-1:111111111111:jobs"
      }, {
      Action   = "logs:PutLogEvents"
      Effect   = "Allow"
      Resource = ["arn:aws:logs:eu-central-1:111111111111:log-group:/aws/lambda/app:*", "arn:aws:logs:eu-central-1:111111111111:log-group:/aws/lambda/app-other:*"]
    }]
    Version = "2012-10-17"
  })
  role = "app-lambda"
}

resource "aws_sqs_queue" "jobs" {
  name                       = "jobs"
  visibility_timeout_seconds = 30
}

resource "aws_cloudwatch_log_group" "aws_lambda_app" {
  name              = "/aws/lambda/app"
  retention_in_days = 14
}

resource "aws_lambda_function" "app" {
  function_name = "app"
  handler       = "index.handler"
  role          = "arn:aws:iam::111111111111:role/app-lambda"
  runtime       = "nodejs22.x"
  image_uri     = null
  vpc_config {
    security_group_ids = ["sg-0aaaaaaaaaaaaaaaa"]
    subnet_ids         = ["subnet-0bbbbbbbbbbbbbbbb"]
  }
}

resource "aws_lambda_event_source_mapping" "app_jobs" {
  batch_size       = 10
  event_source_arn = "arn:aws:sqs:eu-central-1:111111111111:jobs"
  function_name    = "arn:aws:lambda:eu-central-1:111111111111:function:app"
}

resource "aws_security_group" "a" {
  name   = "a"
  vpc_id = "vpc-0ccccccccccccccc0"
  ingress = [{
    cidr_blocks     = []
    from_port       = 443
    protocol        = "tcp"
    security_groups = ["sg-0dddddddddddddddd"]
    self            = false
    to_port         = 443
  }]
}

resource "aws_security_group" "b" {
  name   = "b"
  vpc_id = "vpc-0ccccccccccccccc0"
  ingress = [{
    cidr_blocks     = []
    from_port       = 5432
    protocol        = "tcp"
    security_groups = ["sg-0aaaaaaaaaaaaaaaa"]
    self            = false
    to_port         = 5432
  }]
}

resource "aws_s3_bucket" "data" {
  bucket = "data-bucket"
}

resource "aws_s3_bucket_policy" "data" {
  bucket = "data-bucket"
  policy = jsonencode({
    Statement = [{
      Action = "s3:GetObject"
      Effect = "Allow"
      Principal = {
        AWS = "arn:aws:iam::111111111111:role/app-lambda"
      }
      Resource = "arn:aws:s3:::data-bucket/*"
    }]
    Version = "2012-10-17"
  })
}

resource "aws_ecs_task_definition" "web" {
  family = "web"
  container_definitions = jsonencode([{
    image = "111111111111.dkr.ecr.eu-central-1.amazonaws.com/web:latest"
    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group  = "/aws/lambda/app"
        awslogs-region = "eu-central-1"
      }
    }
    name = "web"
  }])
  execution_role_arn = "arn:aws:iam::111111111111:role/app-lambda"
}
