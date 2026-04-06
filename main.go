package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/landakram/plaid-cli/pkg/plaid_cli"
	"github.com/manifoldco/promptui"
	plaid "github.com/plaid/plaid-go/plaid"
	"github.com/spf13/cobra"

	"github.com/spf13/viper"

	"github.com/Xuanwo/go-locale"
	"golang.org/x/text/language"
)

func sliceToMap(slice []string) map[string]bool {
	set := make(map[string]bool, len(slice))
	for _, s := range slice {
		set[s] = true
	}
	return set
}

// See https://plaid.com/docs/link/customization/#language-and-country
var plaidSupportedCountries = []string{"US", "CA", "GB", "IE", "ES", "FR", "NL"}
var plaidSupportedLanguages = []string{"en", "fr", "es", "nl"}

func AreValidCountries(countries []string) bool {
	supportedCountries := sliceToMap(plaidSupportedCountries)
	for _, c := range countries {
		if !supportedCountries[c] {
			return false
		}
	}

	return true
}

func IsValidLanguageCode(lang string) bool {
	supportedLanguages := sliceToMap(plaidSupportedLanguages)
	return supportedLanguages[lang]
}

func main() {
	log.SetFlags(0)

	usr, _ := user.Current()
	dir := usr.HomeDir
	viper.SetDefault("cli.data_dir", filepath.Join(dir, ".plaid-cli"))

	dataDir := viper.GetString("cli.data_dir")
	data, err := plaid_cli.LoadData(dataDir)

	if err != nil {
		log.Fatal(DescribePlaidError(err))
	}

	viper.SetConfigName("config")
	viper.SetConfigType("toml")
	viper.AddConfigPath(dataDir)
	viper.AddConfigPath(".")
	err = viper.ReadInConfig()
	if err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found; ignore error if desired
		} else {
			log.Fatal(DescribePlaidError(err))
		}
	}

	viper.SetEnvPrefix("")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	viper.AutomaticEnv()

	tag, err := locale.Detect()
	if err != nil {
		tag = language.AmericanEnglish
	}

	region, _ := tag.Region()
	base, _ := tag.Base()

	var country string
	if region.IsCountry() {
		country = region.String()
	} else {
		country = "US"
	}

	lang := base.String()

	viper.SetDefault("plaid.countries", []string{country})
	countriesOpt := viper.GetStringSlice("plaid.countries")
	var countries []string
	for _, c := range countriesOpt {
		countries = append(countries, strings.ToUpper(c))
	}

	viper.SetDefault("plaid.language", lang)
	lang = viper.GetString("plaid.language")

	if !AreValidCountries(countries) {
		log.Fatalln("⚠️  Invalid countries. Please configure `plaid.countries` (using an envvar, PLAID_COUNTRIES, or in plaid-cli's config file) to a subset of countries that Plaid supports. Plaid supports the following countries: ", plaidSupportedCountries)
	}

	if !IsValidLanguageCode(lang) {
		log.Fatalln("⚠️  Invalid language code. Please configure `plaid.language` (using an envvar, PLAID_LANGUAGE, or in plaid-cli's config file) to a language that Plaid supports. Plaid supports the following languages: ", plaidSupportedLanguages)
	}

	plaidCountries, err := PlaidCountryCodes(countries)
	if err != nil {
		log.Fatalln(DescribePlaidError(err))
	}

	viper.SetDefault("plaid.environment", "sandbox")
	plaidEnvStr := strings.ToLower(viper.GetString("plaid.environment"))

	var plaidEnv plaid.Environment
	switch plaidEnvStr {
	case "sandbox":
		plaidEnv = plaid.Sandbox
	case "production":
		plaidEnv = plaid.Production
	case "development":
		log.Fatalln("Invalid plaid environment: 'development' has been decommissioned. Use 'sandbox' or 'production'.")
	default:
		log.Fatalln("Invalid plaid environment. Valid plaid environments are 'sandbox' or 'production'.")
	}

	configuration := plaid.NewConfiguration()
	configuration.AddDefaultHeader("PLAID-CLIENT-ID", viper.GetString("plaid.client_id"))
	configuration.AddDefaultHeader("PLAID-SECRET", viper.GetString("plaid.secret"))
	configuration.UseEnvironment(plaidEnv)
	configuration.HTTPClient = &http.Client{}

	client := plaid.NewAPIClient(configuration)
	linker := plaid_cli.NewLinker(data, client, plaidCountries, lang)

	linkCommand := &cobra.Command{
		Use:   "link [ITEM-ID-OR-ALIAS]",
		Short: "Link an institution so plaid-cli can pull transactions",
		Long:  "Link an institution so plaid-cli can pull transactions. An item ID or alias can be passed to initiate a relink.",
		Args:  cobra.MaximumNArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			port := viper.GetString("link.port")

			var tokenPair *plaid_cli.TokenPair

			var err error

			if len(args) > 0 && len(args[0]) > 0 {
				itemOrAlias := args[0]

				itemID, ok := data.Aliases[itemOrAlias]
				if ok {
					itemOrAlias = itemID
				}

				err = linker.Relink(itemOrAlias, port)
				log.Println("Institution relinked!")
				return
			} else {
				tokenPair, err = linker.Link(port)
				if err != nil {
					log.Fatalln(DescribePlaidError(err))
				}
				data.Tokens[tokenPair.ItemID] = tokenPair.AccessToken
				err = data.Save()
			}

			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}

			log.Println("Institution linked!")
			log.Println(fmt.Sprintf("Item ID: %s", tokenPair.ItemID))

			if alias, ok := data.BackAliases[tokenPair.ItemID]; ok {
				log.Println(fmt.Sprintf("Alias: %s", alias))
				return
			}

			validate := func(input string) error {
				matched, err := regexp.Match(`^\w+$`, []byte(input))
				if err != nil {
					return err
				}

				if !matched && input != "" {
					return errors.New("Valid characters: [0-9A-Za-z_]")
				}

				return nil
			}

			log.Println("You can give the institution a friendly alias and use that instead of the item ID in most commands.")
			prompt := promptui.Prompt{
				Label:    "Alias (default: none)",
				Validate: validate,
			}

			input, err := prompt.Run()
			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}

			if input != "" {
				err = SetAlias(data, tokenPair.ItemID, input)
				if err != nil {
					log.Fatalln(DescribePlaidError(err))
				}
			}
		},
	}

	linkCommand.Flags().StringP("port", "p", "8080", "Port on which to serve Plaid Link")
	viper.BindPFlag("link.port", linkCommand.Flags().Lookup("port"))

	tokensCommand := &cobra.Command{
		Use:   "tokens",
		Short: "List access tokens",
		Run: func(cmd *cobra.Command, args []string) {
			resolved := make(map[string]string)
			for itemID, token := range data.Tokens {
				if alias, ok := data.BackAliases[itemID]; ok {
					resolved[alias] = token
				} else {
					resolved[itemID] = token
				}
			}

			printJSON, err := json.MarshalIndent(resolved, "", "  ")
			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
			fmt.Println(string(printJSON))
		},
	}

	aliasCommand := &cobra.Command{
		Use:   "alias [ITEM-ID] [NAME]",
		Short: "Give a linked institution a friendly name",
		Long:  "Give a linked institution a friendly name. You can use this name instead of the idem ID in most commands.",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			itemID := args[0]
			alias := args[1]

			err := SetAlias(data, itemID, alias)
			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
		},
	}

	aliasesCommand := &cobra.Command{
		Use:   "aliases",
		Short: "List aliases",
		Run: func(cmd *cobra.Command, args []string) {
			printJSON, err := json.MarshalIndent(data.Aliases, "", "  ")
			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
			fmt.Println(string(printJSON))
		},
	}

	accountsCommand := &cobra.Command{
		Use:   "accounts [ITEM-ID-OR-ALIAS]",
		Short: "List accounts for a given institution",
		Long:  "List accounts for a given institution. An account ID returned from this command can be used as a filter when listing transactions.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemOrAlias := args[0]
			itemID, ok := data.Aliases[itemOrAlias]
			if ok {
				itemOrAlias = itemID
			}

			err := WithRelinkOnAuthError(itemOrAlias, data, linker, func() error {
				token, err := AccessTokenForItem(data, itemOrAlias)
				if err != nil {
					return err
				}

				res, _, err := client.PlaidApi.AccountsGet(context.Background()).
					AccountsGetRequest(*plaid.NewAccountsGetRequest(token)).
					Execute()
				if err != nil {
					return err
				}

				b, err := json.MarshalIndent(res.GetAccounts(), "", "  ")
				if err != nil {
					return err
				}

				fmt.Println(string(b))

				return nil
			})

			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
		},
	}

	var fromFlag string
	var toFlag string
	var accountID string
	var outputFormat string
	transactionsCommand := &cobra.Command{
		Use:   "transactions [ITEM-ID-OR-ALIAS]",
		Short: "List transactions for a given institution",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemOrAlias := args[0]
			itemID, ok := data.Aliases[itemOrAlias]
			if ok {
				itemOrAlias = itemID
			}

			err := WithRelinkOnAuthError(itemOrAlias, data, linker, func() error {
				token, err := AccessTokenForItem(data, itemOrAlias)
				if err != nil {
					return err
				}

				var accountIDs []string
				if len(accountID) > 0 {
					accountIDs = append(accountIDs, accountID)
				}

				options := plaid.NewTransactionsGetRequestOptions()
				options.SetCount(100)
				options.SetOffset(0)
				if len(accountIDs) > 0 {
					options.SetAccountIds(accountIDs)
				}

				transactions, err := AllTransactions(*options, client, token, fromFlag, toFlag)
				if err != nil {
					return err
				}

				serializer, err := NewTransactionSerializer(outputFormat)
				if err != nil {
					return err
				}

				b, err := serializer.serialize(transactions)
				if err != nil {
					return err
				}

				fmt.Println(string(b))

				return nil
			})

			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
		},
	}
	transactionsCommand.Flags().StringVarP(&fromFlag, "from", "f", "", "Date of first transaction (required)")
	transactionsCommand.MarkFlagRequired("from")

	transactionsCommand.Flags().StringVarP(&toFlag, "to", "t", "", "Date of last transaction (required)")
	transactionsCommand.MarkFlagRequired("to")

	transactionsCommand.Flags().StringVarP(&outputFormat, "output-format", "o", "json", "Output format")
	transactionsCommand.Flags().StringVarP(&accountID, "account-id", "a", "", "Fetch transactions for this account ID only.")

	var withStatusFlag bool
	var withOptionalMetadataFlag bool
	insitutionCommand := &cobra.Command{
		Use:   "institution [ITEM-ID-OR-ALIAS]",
		Short: "Get information about an institution",
		Long:  "Get information about an institution. Status can be reported using a flag.",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			itemOrAlias := args[0]
			itemID, ok := data.Aliases[itemOrAlias]
			if ok {
				itemOrAlias = itemID
			}

			err := WithRelinkOnAuthError(itemOrAlias, data, linker, func() error {
				token, err := AccessTokenForItem(data, itemOrAlias)
				if err != nil {
					return err
				}

				itemResp, _, err := client.PlaidApi.ItemGet(context.Background()).
					ItemGetRequest(*plaid.NewItemGetRequest(token)).
					Execute()
				if err != nil {
					return err
				}

				item := itemResp.GetItem()
				instID := item.GetInstitutionId()
				if instID == "" {
					return errors.New("institution ID missing from Plaid item")
				}

				opts := plaid.NewInstitutionsGetByIdRequestOptions()
				opts.SetIncludeOptionalMetadata(withOptionalMetadataFlag)
				opts.SetIncludeStatus(withStatusFlag)

				request := plaid.NewInstitutionsGetByIdRequest(instID, plaidCountries)
				request.SetOptions(*opts)

				resp, _, err := client.PlaidApi.InstitutionsGetById(context.Background()).
					InstitutionsGetByIdRequest(*request).
					Execute()
				if err != nil {
					return err
				}

				b, err := json.MarshalIndent(resp.GetInstitution(), "", "  ")
				if err != nil {
					return err
				}

				fmt.Println(string(b))

				return nil
			})

			if err != nil {
				log.Fatalln(DescribePlaidError(err))
			}
		},
	}
	insitutionCommand.Flags().BoolVarP(&withStatusFlag, "status", "s", false, "Fetch institution status")
	insitutionCommand.Flags().BoolVarP(&withOptionalMetadataFlag, "optional-metadata", "m", false, "Fetch optional metadata like logo and URL")

	rootCommand := &cobra.Command{
		Use:   "plaid-cli",
		Short: "Link bank accounts and get transactions from the command line.",
		Long: `plaid-cli 🤑

plaid-cli is a CLI tool for working with the Plaid API.

You can use plaid-cli to link bank accounts and pull transactions in multiple 
output formats from the comfort of the command line.

Configuration:
  To get started, you'll need Plaid API credentials, which you can get by visiting
  https://dashboard.plaid.com/team/keys after signing up for free.
  
  plaid-cli will look at the following environment variables for API credentials:
  
    PLAID_CLIENT_ID=<client id>
    PLAID_SECRET=<sandbox secret>
    PLAID_ENVIRONMENT=sandbox
    PLAID_LANGUAGE=en  # optional, detected using system's locale
    PLAID_COUNTRIES=US # optional, detected using system's locale
  
  I recommend setting and exporting these on shell startup.
  
  API credentials can also be specified using a config file located at 
  ~/.plaid-cli/config.toml:
  
    [plaid]
    client_id = "<client id>"
    secret = "<sandbox secret>"
    environment = "sandbox"
  
  After setting those API credentials, plaid-cli is ready to use! 
  You'll probably want to run 'plaid-cli link' next.
  
  Please see the README (https://github.com/landakram/plaid-cli/blob/master/README.md) 
  for more detailed usage instructions.

  Made by @landakram.
`,
	}
	rootCommand.AddCommand(linkCommand)
	rootCommand.AddCommand(tokensCommand)
	rootCommand.AddCommand(aliasCommand)
	rootCommand.AddCommand(aliasesCommand)
	rootCommand.AddCommand(accountsCommand)
	rootCommand.AddCommand(transactionsCommand)
	rootCommand.AddCommand(insitutionCommand)

	if !viper.IsSet("plaid.client_id") {
		log.Println("⚠️  PLAID_CLIENT_ID not set. Please see the configuration instructions below.")
		rootCommand.Help()
		os.Exit(1)
	}
	if !viper.IsSet("plaid.secret") {
		log.Println("⚠️ PLAID_SECRET not set. Please see the configuration instructions below.")
		rootCommand.Help()
		os.Exit(1)
	}

	if err := rootCommand.Execute(); err != nil {
		log.Fatalln(DescribePlaidError(err))
	}
}

func PlaidCountryCodes(countries []string) ([]plaid.CountryCode, error) {
	plaidCountries := make([]plaid.CountryCode, 0, len(countries))
	for _, country := range countries {
		plaidCountry, err := plaid.NewCountryCodeFromValue(country)
		if err != nil {
			return nil, err
		}

		plaidCountries = append(plaidCountries, *plaidCountry)
	}

	return plaidCountries, nil
}

func AllTransactions(opts plaid.TransactionsGetRequestOptions, client *plaid.APIClient, token string, from string, to string) ([]plaid.Transaction, error) {
	transactions := make([]plaid.Transaction, 0)

	request := plaid.NewTransactionsGetRequest(token, from, to)
	request.SetOptions(opts)

	res, _, err := client.PlaidApi.TransactionsGet(context.Background()).
		TransactionsGetRequest(*request).
		Execute()
	if err != nil {
		return transactions, err
	}

	transactions = append(transactions, res.GetTransactions()...)

	for len(transactions) < int(res.GetTotalTransactions()) {
		opts.SetOffset(opts.GetOffset() + opts.GetCount())

		request := plaid.NewTransactionsGetRequest(token, from, to)
		request.SetOptions(opts)

		res, _, err = client.PlaidApi.TransactionsGet(context.Background()).
			TransactionsGetRequest(*request).
			Execute()
		if err != nil {
			return transactions, err
		}

		transactions = append(transactions, res.GetTransactions()...)

	}

	return transactions, nil
}

func AccessTokenForItem(data *plaid_cli.Data, itemID string) (string, error) {
	token, ok := data.Tokens[itemID]
	if !ok || token == "" {
		return "", fmt.Errorf("No access token found for `%s`. Run `plaid-cli aliases` to see saved aliases, or `plaid-cli link` to create a new link.", itemID)
	}

	return token, nil
}

func DescribePlaidError(err error) error {
	if err == nil {
		return nil
	}

	plaidErr, plaidErrParseErr := plaid.ToPlaidError(err)
	if plaidErrParseErr != nil {
		return err
	}

	message := fmt.Sprintf("%s: %s", plaidErr.GetErrorCode(), plaidErr.GetErrorMessage())

	if requestID := plaidErr.GetRequestId(); requestID != "" {
		message = fmt.Sprintf("%s (request_id: %s)", message, requestID)
	}

	if suggestedAction := plaidErr.GetSuggestedAction(); suggestedAction != "" {
		message = fmt.Sprintf("%s | suggested_action: %s", message, suggestedAction)
	}

	return errors.New(message)
}

func WithRelinkOnAuthError(itemID string, data *plaid_cli.Data, linker *plaid_cli.Linker, action func() error) error {
	err := action()
	if err == nil {
		return nil
	}

	plaidErr, plaidErrParseErr := plaid.ToPlaidError(err)
	if plaidErrParseErr == nil && plaidErr.ErrorCode == "ITEM_LOGIN_REQUIRED" {
		log.Println("Login expired. Relinking...")

		port := viper.GetString("link.port")

		err = linker.Relink(itemID, port)

		if err != nil {
			return err
		}

		log.Println("Re-running action...")

		err = action()
	}

	return err
}

type TransactionSerializer interface {
	serialize(txs []plaid.Transaction) ([]byte, error)
}

func NewTransactionSerializer(t string) (TransactionSerializer, error) {
	switch t {
	case "csv":
		return &CSVSerializer{}, nil
	case "json":
		return &JSONSerializer{}, nil
	default:
		return nil, errors.New(fmt.Sprintf("Invalid output format: %s", t))
	}
}

type CSVSerializer struct{}

func (w *CSVSerializer) serialize(txs []plaid.Transaction) ([]byte, error) {
	var records [][]string
	for _, tx := range txs {
		sanitizedName := strings.ReplaceAll(tx.Name, ",", "")
		records = append(records, []string{tx.Date, fmt.Sprintf("%f", tx.Amount), sanitizedName})
	}

	b := bytes.NewBufferString("")
	writer := csv.NewWriter(b)
	err := writer.Write([]string{"Date", "Amount", "Description"})
	if err != nil {
		return nil, err
	}
	err = writer.WriteAll(records)
	if err != nil {
		return nil, err
	}

	return b.Bytes(), err
}

func SetAlias(data *plaid_cli.Data, itemID string, alias string) error {
	if _, ok := data.Tokens[itemID]; !ok {
		return errors.New(fmt.Sprintf("No access token found for item ID `%s`. Try re-linking your account with `plaid-cli link`.", itemID))
	}

	data.Aliases[alias] = itemID
	data.BackAliases[itemID] = alias
	err := data.Save()
	if err != nil {
		return err
	}

	log.Println(fmt.Sprintf("Aliased %s to %s.", itemID, alias))

	return nil
}

type JSONSerializer struct{}

func (w *JSONSerializer) serialize(txs []plaid.Transaction) ([]byte, error) {
	if txs == nil {
		txs = make([]plaid.Transaction, 0)
	}

	return json.MarshalIndent(txs, "", "  ")
}
