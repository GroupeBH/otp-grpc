# Préparation ECS Fargate

Aucune ressource AWS n'a été créée. Les deux fichiers JSON sont des modèles à intégrer au Terraform existant dans `zwanga-backend/infra-aws`, pas des définitions déployées.

## Ressources à créer ou réutiliser

| Élément | Décision |
| --- | --- |
| ECR | Créer un dépôt dédié `zwanga-otp` avec scan et chiffrement KMS, comme le dépôt backend existant. |
| VPC, sous-réseaux privés, cluster ECS | Réutiliser les ressources existantes du backend. |
| Namespace Cloud Map | Créer `zwanga.internal` si absent, sinon réutiliser son ARN. |
| Service ECS | Créer `zwanga-otp` avec tâches Fargate distinctes du monolithe. |
| Port | Déclarer `otp-grpc` sur 50051 avec `appProtocol: grpc` dans la Task Definition. |
| Redis | Réutiliser l'instance partagée si capacité, isolation des clés et haute disponibilité sont adéquates ; conserver le transport NestJS ↔ Go sur gRPC. |
| Secrets Manager | Fournir `OTP_HASH_KEY`, `OTP_SERVICE_TOKEN`, identifiants Mobishastra et secret Redis par ARN. Ne pas stocker les valeurs dans Terraform. |
| IAM | Rôle d'exécution ECS pour ECR, CloudWatch et lecture ciblée des secrets ; rôle de tâche limité aux ressources réellement utilisées. |
| Security Groups | Autoriser le backend vers le port 50051 du service OTP, le service OTP vers Redis et la sortie HTTPS vers Mobishastra via NAT. Pas d'ingress public sur 50051. |
| CloudWatch | Créer `/ecs/zwanga-otp`, définir rétention et alarmes sur erreurs et tâches non saines. |

Le service OTP configure l'alias `otp-service:50051` dans `serviceConnectConfiguration`, selon [les paramètres officiels ECS](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/service-connect-concepts.html). Le service ECS du monolithe rejoint le même namespace en mode client et utilise `OTP_GRPC_TARGET=otp-service:50051`. Service Connect ajoute un proxy aux tâches ; prévoir sa mémoire dans le dimensionnement. Les modèles JSON illustrent la configuration attendue. Le Terraform du backend doit intégrer le service client dans sa ressource ECS existante, sans créer un second service NestJS.

## TLS et identité

En local, gRPC circule sur le réseau privé Compose. En production, activer [TLS Service Connect avec AWS Private CA](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/service-connect-tls.html) pour le service OTP, avec le rôle d'infrastructure ECS et les permissions correspondantes. ECS gère les certificats et leur rotation. TLS est établi entre les proxies Service Connect : l'application ne voit pas cette connexion TLS et le trafic qui contourne le proxy n'est pas couvert. Conserver `OTP_SERVICE_TOKEN` pour authentifier NestJS auprès de Go et prévoir sa rotation coordonnée ; restreindre aussi les Security Groups. Ne pas activer de TLS applicatif fictif tant que les certificats et le chemin réseau ne sont pas préparés.

## Déploiement ultérieur

1. Ajouter les ressources au Terraform existant, faire `terraform fmt`, `validate` et `plan`, puis examiner le plan avant tout `apply`.
2. Créer les secrets et vérifier les crédits, le Sender ID et la couverture RDC du compte Mobishastra. Construire l'image Go avec un tag immuable, la pousser dans ECR.
3. Déployer Redis et le service OTP avec `OTP_GRPC_ENABLED=false` côté backend. Vérifier les health checks et les appels gRPC dans un environnement de préproduction.
4. Après expiration des codes Keccel en cours, activer `OTP_GRPC_ENABLED=true` progressivement sur le backend. Suivre taux d'envoi, refus, latence et erreurs gRPC.
5. En cas d'incident, remettre `OTP_GRPC_ENABLED=false` puis redéployer le backend ; les nouveaux codes Go ne sont pas vérifiables par Keccel. Maintenir une fenêtre de transition contrôlée et informer les utilisateurs concernés si nécessaire.

Paramètres initiaux à confirmer par mesure : tâche 512 CPU / 1024 MiB, une tâche minimum dans plusieurs zones, déploiement avec circuit breaker et rollback, autoscaling sur CPU et latence lorsque le trafic le justifie. Les secrets et le réseau doivent être validés avant le déploiement réel.
