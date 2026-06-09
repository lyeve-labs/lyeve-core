package i18n

// translations is the canonical message catalog.
// Outer key: error code. Inner key: locale primary subtag.
// Fallback chain is applied by Load().
var translations = map[Code]map[string]string{
	// Generic
	CodeInternalError: {
		"en": "An internal error occurred. Please try again later.",
		"fr": "Une erreur interne s'est produite. Veuillez réessayer plus tard.",
	},
	CodeInvalidBody: {
		"en": "The request body is invalid or could not be parsed.",
		"fr": "Le corps de la requête est invalide ou n'a pas pu être analysé.",
	},
	CodeNotFound: {
		"en": "The requested resource was not found.",
		"fr": "La ressource demandée est introuvable.",
	},
	CodeForbidden: {
		"en": "You do not have permission to perform this action.",
		"fr": "Vous n'avez pas l'autorisation d'effectuer cette action.",
	},
	CodeUnauthorized: {
		"en": "Authentication is required to access this resource.",
		"fr": "Une authentification est requise pour accéder à cette ressource.",
	},
	CodeRateLimited: {
		"en": "Too many requests. Please slow down and try again later.",
		"fr": "Trop de requêtes. Veuillez ralentir et réessayer plus tard.",
	},
	CodeValidationFailed: {
		"en": "The provided data failed validation.",
		"fr": "Les données fournies n'ont pas passé la validation.",
	},

	// Auth
	CodeInvalidCredentials: {
		"en": "Invalid email or password.",
		"fr": "Email ou mot de passe invalide.",
	},
	CodeSetupAlreadyComplete: {
		"en": "The initial setup has already been completed.",
		"fr": "La configuration initiale a déjà été effectuée.",
	},
	CodeSetupModeRequired: {
		"en": "The engine is in setup mode and serves nothing until its configuration is complete. Open the admin to see what to set.",
		"fr": "Le moteur est en mode configuration et ne sert rien tant que sa configuration n'est pas complète. Ouvrez l'administration pour voir quoi définir.",
	},
	CodeSetupTokenInvalid: {
		"en": "A valid setup token is required. Use LYEVE_SETUP_TOKEN, or the token the engine printed to its log at startup.",
		"fr": "Un jeton de configuration valide est requis. Utilisez LYEVE_SETUP_TOKEN, ou le jeton que le moteur a écrit dans son journal au démarrage.",
	},
	CodePasswordTooShort: {
		"en": "Password must be at least {min} characters.",
		"fr": "Le mot de passe doit comporter au moins {min} caractères.",
	},
	CodePasswordHashFailed: {
		"en": "Failed to process the password. Please try again.",
		"fr": "Échec du traitement du mot de passe. Veuillez réessayer.",
	},
	CodePasswordNeedsComplexity: {
		"en": "Password must contain at least one uppercase letter, one lowercase letter, and one digit.",
		"fr": "Le mot de passe doit contenir au moins une lettre majuscule, une lettre minuscule et un chiffre.",
	},
	CodePasswordTooCommon: {
		"en": "This password is too common. Please choose a more unique password.",
		"fr": "Ce mot de passe est trop courant. Veuillez choisir un mot de passe plus unique.",
	},
	CodeUserCreateFailed: {
		"en": "Failed to create the user account.",
		"fr": "Échec de la création du compte utilisateur.",
	},
	CodeTokenSignFailed: {
		"en": "Failed to issue an authentication token.",
		"fr": "Échec de l'émission du jeton d'authentification.",
	},
	CodeChallengeFailed: {
		"en": "Failed to issue the MFA challenge.",
		"fr": "Échec de l'émission du défi MFA.",
	},
	CodeChallengeExpired: {
		"en": "The MFA challenge has expired. Please log in again.",
		"fr": "Le défi MFA a expiré. Veuillez vous reconnecter.",
	},
	CodeMFANotConfigured: {
		"en": "Multi-factor authentication is not configured for this account.",
		"fr": "L'authentification multi-facteurs n'est pas configurée pour ce compte.",
	},
	CodeMFAInvalidCode: {
		"en": "The provided verification code is invalid.",
		"fr": "Le code de vérification fourni est invalide.",
	},
	CodeTokenReuse: {
		"en": "Refresh token reuse detected. This session has been revoked for security.",
		"fr": "Réutilisation du jeton détectée. Cette session a été révoquée par sécurité.",
	},
	CodeTokenExpired: {
		"en": "Your session has expired. Please log in again.",
		"fr": "Votre session a expiré. Veuillez vous reconnecter.",
	},
	CodeSessionRevoked: {
		"en": "This session has been revoked.",
		"fr": "Cette session a été révoquée.",
	},
	CodeRefreshNotEnabled: {
		"en": "Refresh tokens are not enabled on this server.",
		"fr": "Les jetons de rafraîchissement ne sont pas activés sur ce serveur.",
	},
	CodeMFANotAvailable: {
		"en": "Multi-factor authentication is not available on this server.",
		"fr": "L'authentification multi-facteurs n'est pas disponible sur ce serveur.",
	},
	CodeAccountLocked: {
		"en": "This account has been temporarily locked due to too many failed login attempts. Please try again later.",
		"fr": "Ce compte a été temporairement verrouillé en raison d'un trop grand nombre de tentatives de connexion échouées. Veuillez réessayer plus tard.",
	},
	CodeAccountDisabled: {
		"en": "This account has been disabled. Please contact your administrator.",
		"fr": "Ce compte a été désactivé. Veuillez contacter votre administrateur.",
	},
	CodeAccountExpired: {
		"en": "This account has expired. Please contact your administrator.",
		"fr": "Ce compte a expiré. Veuillez contacter votre administrateur.",
	},
	CodeMFAStoreError: {
		"en": "Multi-factor authentication is temporarily unavailable. Please try again later.",
		"fr": "L'authentification multi-facteurs est temporairement indisponible. Veuillez réessayer plus tard.",
	},
	CodeMFALockedOut: {
		"en": "Too many MFA verification attempts. Please log in again to receive a new challenge.",
		"fr": "Trop de tentatives de vérification MFA. Veuillez vous reconnecter pour obtenir un nouveau défi.",
	},

	// Users
	CodeUserNotFound: {
		"en": "The requested user was not found.",
		"fr": "L'utilisateur demandé est introuvable.",
	},
	CodeInvalidUserID: {
		"en": "The user ID is not valid.",
		"fr": "L'identifiant utilisateur n'est pas valide.",
	},
	CodeRolesRequired: {
		"en": "At least one role is required.",
		"fr": "Au moins un rôle est requis.",
	},

	// Tenant features
	CodeTenantFeatureWithheld: {
		"en": "This feature is not available to this tenant.",
		"fr": "Cette fonctionnalité n'est pas disponible pour ce locataire.",
	},
	CodeTenantFeatureUnknown: {
		"en": "{name} is not a plugin this instance serves.",
		"fr": "{name} n'est pas un plugin servi par cette instance.",
	},
	CodeTenantSlugInvalid: {
		"en": "The tenant must be a slug: a lower-case letter, then letters, digits or underscores.",
		"fr": "Le locataire doit être un identifiant : une lettre minuscule, puis des lettres, des chiffres ou des tirets bas.",
	},

	// Schema
	CodeSchemaNotFound: {
		"en": "The requested schema was not found.",
		"fr": "Le schéma demandé est introuvable.",
	},
	CodeMigrationFailed: {
		"en": "The database migration failed. Check the server logs for details.",
		"fr": "La migration de base de données a échoué. Consultez les journaux du serveur pour plus de détails.",
	},

	// Content
	CodeContentNotFound: {
		"en": "The requested content was not found.",
		"fr": "Le contenu demandé est introuvable.",
	},

	// DB
	CodeDatabaseError: {
		"en": "A database error occurred. Please try again later.",
		"fr": "Une erreur de base de données s'est produite. Veuillez réessayer plus tard.",
	},

	// Availability
	CodeSessionStoreUnavailable: {
		"en": "The session store is temporarily unavailable. Please try again.",
		"fr": "Le magasin de sessions est temporairement indisponible. Veuillez réessayer.",
	},

	// Validation
	CodeValidationRequired: {
		"en": "The field {field} is required.",
		"fr": "Le champ {field} est obligatoire.",
	},
	CodeValidationMinLength: {
		"en": "The field {field} must be at least {min} characters.",
		"fr": "Le champ {field} doit comporter au moins {min} caractères.",
	},
	CodeValidationMaxLength: {
		"en": "The field {field} must not exceed {max} characters.",
		"fr": "Le champ {field} ne doit pas dépasser {max} caractères.",
	},
	CodeValidationMinValue: {
		"en": "The field {field} must be at least {min}.",
		"fr": "Le champ {field} doit être au moins {min}.",
	},
	CodeValidationMaxValue: {
		"en": "The field {field} must not exceed {max}.",
		"fr": "Le champ {field} ne doit pas dépasser {max}.",
	},
	CodeValidationFieldType: {
		"en": "The field {field} must be of type {expected}.",
		"fr": "Le champ {field} doit être de type {expected}.",
	},
	CodeValidationEmail: {
		"en": "The field {field} must be a valid email address.",
		"fr": "Le champ {field} doit être une adresse email valide.",
	},
	CodeValidationUUID: {
		"en": "The field {field} must be a valid UUID.",
		"fr": "Le champ {field} doit être un UUID valide.",
	},
	CodeValidationURL: {
		"en": "The field {field} must be a valid URL.",
		"fr": "Le champ {field} doit être une URL valide.",
	},
	CodeValidationTenantSlug: {
		"en": "The field {field} must be a valid tenant slug (lowercase letters, digits, and hyphens; 3-63 characters).",
		"fr": "Le champ {field} doit être un identifiant de locataire valide (lettres minuscules, chiffres et tirets; 3-63 caractères).",
	},
	CodeValidationStrongPassword: {
		"en": "The field {field} must contain at least one uppercase letter, one lowercase letter, one digit, and one special character.",
		"fr": "Le champ {field} doit contenir au moins une lettre majuscule, une lettre minuscule, un chiffre et un caractère spécial.",
	},
	CodeValidationUnknownField: {
		"en": "The field {field} failed validation.",
		"fr": "Le champ {field} n'a pas passé la validation.",
	},

	// Schema Validation
	CodeSchemaValidationEmail: {
		"en": "The field {field} must be a valid email address.",
		"fr": "Le champ {field} doit être une adresse email valide.",
	},
	CodeSchemaValidationURL: {
		"en": "The field {field} must be a valid URL.",
		"fr": "Le champ {field} doit être une URL valide.",
	},
	CodeSchemaValidationRegex: {
		"en": "The field {field} does not match the required pattern.",
		"fr": "Le champ {field} ne correspond pas au motif requis.",
	},
	CodeSchemaValidationEnum: {
		"en": "The field {field} must be one of: {values}.",
		"fr": "Le champ {field} doit être l'une des valeurs suivantes : {values}.",
	},
	CodeSchemaValidationMin: {
		"en": "The field {field} must be at least {min}.",
		"fr": "Le champ {field} doit être au moins {min}.",
	},
	CodeSchemaValidationMax: {
		"en": "The field {field} must not exceed {max}.",
		"fr": "Le champ {field} ne doit pas dépasser {max}.",
	},
	CodeSchemaValidationRequiredWith: {
		"en": "The field {field} is required when {other} is present.",
		"fr": "Le champ {field} est obligatoire lorsque {other} est présent.",
	},
	CodeSchemaValidationLtField: {
		"en": "The field {field} must be less than {other}.",
		"fr": "Le champ {field} doit être inférieur à {other}.",
	},
	CodeSchemaValidationGtField: {
		"en": "The field {field} must be greater than {other}.",
		"fr": "Le champ {field} doit être supérieur à {other}.",
	},
	CodeSchemaValidationLteField: {
		"en": "The field {field} must be less than or equal to {other}.",
		"fr": "Le champ {field} doit être inférieur ou égal à {other}.",
	},
	CodeSchemaValidationGteField: {
		"en": "The field {field} must be greater than or equal to {other}.",
		"fr": "Le champ {field} doit être supérieur ou égal à {other}.",
	},
	CodeSchemaValidationRange: {
		"en": "The field {field} must be between {min} and {max}.",
		"fr": "Le champ {field} doit être compris entre {min} et {max}.",
	},
	CodeSchemaValidationCustom: {
		"en": "The field {field} failed custom validation: {error}.",
		"fr": "Le champ {field} n'a pas passé la validation personnalisée : {error}.",
	},
	CodeSchemaValidationCrossField: {
		"en": "Cross-field validation failed: {rule}.",
		"fr": "La validation inter-champs a échoué : {rule}.",
	},
}

// Load returns the best available translation for the given code and locale,
// walking the fallback chain (e.g. fr-CA -> fr -> en). Returns the raw code
// string when no translation exists.
func Load(code Code, loc Locale) string {
	entries, ok := translations[code]
	if !ok {
		return string(code)
	}

	primary := loc.Primary()
	chain := fallbackChain[primary]
	if chain == nil {
		chain = []string{primary, "en"}
	}

	for _, lang := range chain {
		if msg, ok := entries[lang]; ok {
			return msg
		}
	}

	return "An error occurred."
}
