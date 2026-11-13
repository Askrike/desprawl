# One map of several providers: each provider's Terraform (here two other
# examples, normally one reverse-engineered workspace per provider) is a local
# module, which desprawl expands in place.

module "aws" {
  source = "../aws-apigw-lambda-dynamodb"
}

module "auth0" {
  source = "../auth0-spa-m2m-api"
}
