// Copyright (c) Microsoft Corporation. All rights reserved.
// Licensed under the MIT license.

package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template" //log "github.com/sirupsen/logrus"

	"github.com/Azure/aks-engine/pkg/api"
	"github.com/Azure/aks-engine/pkg/helpers"
	"github.com/pkg/errors"
)

var commonTemplateFiles = []string{agentOutputs, agentParams, masterOutputs, iaasOutputs, masterParams, windowsParams}
var kubernetesTemplateFiles = []string{kubernetesBaseFile, kubernetesAgentResourcesVMAS, kubernetesAgentResourcesVMSS, kubernetesAgentVars, kubernetesMasterResourcesVMAS, kubernetesMasterResourcesVMSS, kubernetesMasterVars, kubernetesParams, kubernetesWinAgentVars, kubernetesWinAgentVarsVMSS}

var keyvaultSecretPathRe *regexp.Regexp

func init() {
	keyvaultSecretPathRe = regexp.MustCompile(`^(/subscriptions/\S+/resourceGroups/\S+/providers/Microsoft.KeyVault/vaults/\S+)/secrets/([^/\s]+)(/(\S+))?$`)
}

// GenerateKubeConfig returns a JSON string representing the KubeConfig
func GenerateKubeConfig(properties *api.Properties, location string) (string, error) {
	if properties == nil {
		return "", errors.New("Properties nil in GenerateKubeConfig")
	}
	if properties.CertificateProfile == nil {
		return "", errors.New("CertificateProfile property may not be nil in GenerateKubeConfig")
	}
	b, err := Asset(kubeConfigJSON)
	if err != nil {
		return "", errors.Wrapf(err, "error reading kube config template file %s", kubeConfigJSON)
	}
	kubeconfig := string(b)
	// variable replacement
	kubeconfig = strings.Replace(kubeconfig, "{{WrapAsVerbatim \"parameters('caCertificate')\"}}", base64.StdEncoding.EncodeToString([]byte(properties.CertificateProfile.CaCertificate)), -1)
	if properties.OrchestratorProfile != nil &&
		properties.OrchestratorProfile.KubernetesConfig != nil &&
		properties.OrchestratorProfile.KubernetesConfig.PrivateCluster != nil &&
		helpers.IsTrueBoolPointer(properties.OrchestratorProfile.KubernetesConfig.PrivateCluster.Enabled) {
		if properties.MasterProfile.Count > 1 {
			// more than 1 master, use the internal lb IP
			firstMasterIP := net.ParseIP(properties.MasterProfile.FirstConsecutiveStaticIP).To4()
			if firstMasterIP == nil {
				return "", errors.Errorf("MasterProfile.FirstConsecutiveStaticIP '%s' is an invalid IP address", properties.MasterProfile.FirstConsecutiveStaticIP)
			}
			lbIP := net.IP{firstMasterIP[0], firstMasterIP[1], firstMasterIP[2], firstMasterIP[3] + byte(DefaultInternalLbStaticIPOffset)}
			kubeconfig = strings.Replace(kubeconfig, "{{WrapAsVerbatim \"reference(concat('Microsoft.Network/publicIPAddresses/', variables('masterPublicIPAddressName'))).dnsSettings.fqdn\"}}", lbIP.String(), -1)
		} else {
			// Master count is 1, use the master IP
			kubeconfig = strings.Replace(kubeconfig, "{{WrapAsVerbatim \"reference(concat('Microsoft.Network/publicIPAddresses/', variables('masterPublicIPAddressName'))).dnsSettings.fqdn\"}}", properties.MasterProfile.FirstConsecutiveStaticIP, -1)
		}
	} else {
		kubeconfig = strings.Replace(kubeconfig, "{{WrapAsVerbatim \"reference(concat('Microsoft.Network/publicIPAddresses/', variables('masterPublicIPAddressName'))).dnsSettings.fqdn\"}}", api.FormatAzureProdFQDNByLocation(properties.MasterProfile.DNSPrefix, location), -1)
	}
	kubeconfig = strings.Replace(kubeconfig, "{{WrapAsVariable \"resourceGroup\"}}", properties.MasterProfile.DNSPrefix, -1)

	var authInfo string
	if properties.AADProfile == nil {
		authInfo = fmt.Sprintf("{\"client-certificate-data\":\"%v\",\"client-key-data\":\"%v\"}",
			base64.StdEncoding.EncodeToString([]byte(properties.CertificateProfile.KubeConfigCertificate)),
			base64.StdEncoding.EncodeToString([]byte(properties.CertificateProfile.KubeConfigPrivateKey)))
	} else {
		tenantID := properties.AADProfile.TenantID
		if len(tenantID) == 0 {
			tenantID = "common"
		}

		authInfo = fmt.Sprintf("{\"auth-provider\":{\"name\":\"azure\",\"config\":{\"environment\":\"%v\",\"tenant-id\":\"%v\",\"apiserver-id\":\"%v\",\"client-id\":\"%v\"}}}",
			helpers.GetCloudTargetEnv(location),
			tenantID,
			properties.AADProfile.ServerAppID,
			properties.AADProfile.ClientAppID)
	}
	kubeconfig = strings.Replace(kubeconfig, "{{authInfo}}", authInfo, -1)

	return kubeconfig, nil
}

// validateDistro checks if the requested orchestrator type is supported on the requested Linux distro.
func validateDistro(cs *api.ContainerService) bool {
	// Check Master distro
	if cs.Properties.MasterProfile != nil && cs.Properties.MasterProfile.Distro == api.RHEL {
		log.Fatalf("Orchestrator type %s not suported on RHEL Master", cs.Properties.OrchestratorProfile.OrchestratorType)
		return false
	}
	// Check Agent distros
	for _, agentProfile := range cs.Properties.AgentPoolProfiles {
		if agentProfile.Distro == api.RHEL {
			log.Fatalf("Orchestrator type %s not suported on RHEL Agent", cs.Properties.OrchestratorProfile.OrchestratorType)
			return false
		}
	}
	return true
}

func addValue(m paramsMap, k string, v interface{}) {
	m[k] = paramsMap{
		"value": v,
	}
}

func addKeyvaultReference(m paramsMap, k string, vaultID, secretName, secretVersion string) {
	m[k] = paramsMap{
		"reference": &KeyVaultRef{
			KeyVault: KeyVaultID{
				ID: vaultID,
			},
			SecretName:    secretName,
			SecretVersion: secretVersion,
		},
	}
}

func addSecret(m paramsMap, k string, v interface{}, encode bool) {
	str, ok := v.(string)
	if !ok {
		addValue(m, k, v)
		return
	}
	parts := keyvaultSecretPathRe.FindStringSubmatch(str)
	if parts == nil || len(parts) != 5 {
		if encode {
			addValue(m, k, base64.StdEncoding.EncodeToString([]byte(str)))
		} else {
			addValue(m, k, str)
		}
		return
	}
	addKeyvaultReference(m, k, parts[1], parts[2], parts[4])
}

// getStorageAccountType returns the support managed disk storage tier for a give VM size
func getStorageAccountType(sizeName string) (string, error) {
	spl := strings.Split(sizeName, "_")
	if len(spl) < 2 {
		return "", errors.Errorf("Invalid sizeName: %s", sizeName)
	}
	capability := spl[1]
	if strings.Contains(strings.ToLower(capability), "s") {
		return "Premium_LRS", nil
	}
	return "Standard_LRS", nil
}

func makeMasterExtensionScriptCommands(cs *api.ContainerService) string {
	copyIndex := "',copyIndex(),'"
	if cs.Properties.OrchestratorProfile.IsKubernetes() {
		copyIndex = "',copyIndex(variables('masterOffset')),'"
	}
	return makeExtensionScriptCommands(cs.Properties.MasterProfile.PreprovisionExtension,
		cs.Properties.ExtensionProfiles, copyIndex)
}

func makeAgentExtensionScriptCommands(cs *api.ContainerService, profile *api.AgentPoolProfile) string {
	copyIndex := "',copyIndex(),'"
	if profile.IsAvailabilitySets() {
		copyIndex = fmt.Sprintf("',copyIndex(variables('%sOffset')),'", profile.Name)
	}
	if profile.OSType == api.Windows {
		return makeWindowsExtensionScriptCommands(profile.PreprovisionExtension,
			cs.Properties.ExtensionProfiles, copyIndex)
	}
	return makeExtensionScriptCommands(profile.PreprovisionExtension,
		cs.Properties.ExtensionProfiles, copyIndex)
}

func makeExtensionScriptCommands(extension *api.Extension, extensionProfiles []*api.ExtensionProfile, copyIndex string) string {
	var extensionProfile *api.ExtensionProfile
	for _, eP := range extensionProfiles {
		if strings.EqualFold(eP.Name, extension.Name) {
			extensionProfile = eP
			break
		}
	}

	if extensionProfile == nil {
		panic(fmt.Sprintf("%s extension referenced was not found in the extension profile", extension.Name))
	}

	extensionsParameterReference := fmt.Sprintf("parameters('%sParameters')", extensionProfile.Name)
	scriptURL := getExtensionURL(extensionProfile.RootURL, extensionProfile.Name, extensionProfile.Version, extensionProfile.Script, extensionProfile.URLQuery)
	scriptFilePath := fmt.Sprintf("/opt/azure/containers/extensions/%s/%s", extensionProfile.Name, extensionProfile.Script)
	return fmt.Sprintf("- sudo /usr/bin/curl --retry 5 --retry-delay 10 --retry-max-time 30 -o %s --create-dirs \"%s\" \n- sudo /bin/chmod 744 %s \n- sudo %s ',%s,' > /var/log/%s-output.log",
		scriptFilePath, scriptURL, scriptFilePath, scriptFilePath, extensionsParameterReference, extensionProfile.Name)
}

func makeWindowsExtensionScriptCommands(extension *api.Extension, extensionProfiles []*api.ExtensionProfile, copyIndex string) string {
	var extensionProfile *api.ExtensionProfile
	for _, eP := range extensionProfiles {
		if strings.EqualFold(eP.Name, extension.Name) {
			extensionProfile = eP
			break
		}
	}

	if extensionProfile == nil {
		panic(fmt.Sprintf("%s extension referenced was not found in the extension profile", extension.Name))
	}

	scriptURL := getExtensionURL(extensionProfile.RootURL, extensionProfile.Name, extensionProfile.Version, extensionProfile.Script, extensionProfile.URLQuery)
	scriptFileDir := fmt.Sprintf("$env:SystemDrive:/AzureData/extensions/%s", extensionProfile.Name)
	scriptFilePath := fmt.Sprintf("%s/%s", scriptFileDir, extensionProfile.Script)
	return fmt.Sprintf("New-Item -ItemType Directory -Force -Path \"%s\" ; Invoke-WebRequest -Uri \"%s\" -OutFile \"%s\" ; powershell \"%s %s\"\n", scriptFileDir, scriptURL, scriptFilePath, scriptFilePath, "$preprovisionExtensionParams")
}

func getVNETAddressPrefixes(properties *api.Properties) string {
	visitedSubnets := make(map[string]bool)
	var buf bytes.Buffer
	buf.WriteString(`"[variables('masterSubnet')]"`)
	visitedSubnets[properties.MasterProfile.Subnet] = true
	for _, profile := range properties.AgentPoolProfiles {
		if _, ok := visitedSubnets[profile.Subnet]; !ok {
			buf.WriteString(fmt.Sprintf(",\n            \"[variables('%sSubnet')]\"", profile.Name))
		}
	}
	return buf.String()
}

func getVNETSubnetDependencies(properties *api.Properties) string {
	agentString := `        "[concat('Microsoft.Network/networkSecurityGroups/', variables('%sNSGName'))]"`
	var buf bytes.Buffer
	for index, agentProfile := range properties.AgentPoolProfiles {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(fmt.Sprintf(agentString, agentProfile.Name))
	}
	return buf.String()
}

func getVNETSubnets(properties *api.Properties, addNSG bool) string {
	masterString := `{
            "name": "[variables('masterSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('masterSubnet')]"
            }
          }`
	agentString := `          {
            "name": "[variables('%sSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('%sSubnet')]"
            }
          }`
	agentStringNSG := `          {
            "name": "[variables('%sSubnetName')]",
            "properties": {
              "addressPrefix": "[variables('%sSubnet')]",
              "networkSecurityGroup": {
                "id": "[resourceId('Microsoft.Network/networkSecurityGroups', variables('%sNSGName'))]"
              }
            }
          }`
	var buf bytes.Buffer
	buf.WriteString(masterString)
	for _, agentProfile := range properties.AgentPoolProfiles {
		buf.WriteString(",\n")
		if addNSG {
			buf.WriteString(fmt.Sprintf(agentStringNSG, agentProfile.Name, agentProfile.Name, agentProfile.Name))
		} else {
			buf.WriteString(fmt.Sprintf(agentString, agentProfile.Name, agentProfile.Name))
		}

	}
	return buf.String()
}

func getLBRule(name string, port int) string {
	return fmt.Sprintf(`	          {
            "name": "LBRule%d",
            "properties": {
              "backendAddressPool": {
                "id": "[concat(variables('%sLbID'), '/backendAddressPools/', variables('%sLbBackendPoolName'))]"
              },
              "backendPort": %d,
              "enableFloatingIP": false,
              "frontendIPConfiguration": {
                "id": "[variables('%sLbIPConfigID')]"
              },
              "frontendPort": %d,
              "idleTimeoutInMinutes": 5,
              "loadDistribution": "Default",
              "probe": {
                "id": "[concat(variables('%sLbID'),'/probes/tcp%dProbe')]"
              },
              "protocol": "tcp"
            }
          }`, port, name, name, port, name, port, name, port)
}

func getLBRules(name string, ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getLBRule(name, port))
	}
	return buf.String()
}

func getProbe(port int) string {
	return fmt.Sprintf(`          {
            "name": "tcp%dProbe",
            "properties": {
              "intervalInSeconds": "5",
              "numberOfProbes": "2",
              "port": %d,
              "protocol": "tcp"
            }
          }`, port, port)
}

func getProbes(ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getProbe(port))
	}
	return buf.String()
}

func getSecurityRule(port int, portIndex int) string {
	// BaseLBPriority specifies the base lb priority.
	BaseLBPriority := 200
	return fmt.Sprintf(`          {
            "name": "Allow_%d",
            "properties": {
              "access": "Allow",
              "description": "Allow traffic from the Internet to port %d",
              "destinationAddressPrefix": "*",
              "destinationPortRange": "%d",
              "direction": "Inbound",
              "priority": %d,
              "protocol": "*",
              "sourceAddressPrefix": "Internet",
              "sourcePortRange": "*"
            }
          }`, port, port, port, BaseLBPriority+portIndex)
}

func getDataDisks(a *api.AgentPoolProfile) string {
	if !a.HasDisks() {
		return ""
	}
	var buf bytes.Buffer
	buf.WriteString("\"dataDisks\": [\n")
	dataDisks := `            {
              "createOption": "Empty",
              "diskSizeGB": "%d",
              "lun": %d,
              "name": "[concat(variables('%sVMNamePrefix'), copyIndex(),'-datadisk%d')]",
              "vhd": {
                "uri": "[concat('http://',variables('storageAccountPrefixes')[mod(add(add(div(copyIndex(),variables('maxVMsPerStorageAccount')),variables('%sStorageAccountOffset')),variables('dataStorageAccountPrefixSeed')),variables('storageAccountPrefixesCount'))],variables('storageAccountPrefixes')[div(add(add(div(copyIndex(),variables('maxVMsPerStorageAccount')),variables('%sStorageAccountOffset')),variables('dataStorageAccountPrefixSeed')),variables('storageAccountPrefixesCount'))],variables('%sDataAccountName'),'.blob.core.windows.net/vhds/',variables('%sVMNamePrefix'),copyIndex(), '--datadisk%d.vhd')]"
              }
            }`
	managedDataDisks := `            {
              "diskSizeGB": "%d",
              "lun": %d,
              "createOption": "Empty"
            }`
	for i, diskSize := range a.DiskSizesGB {
		if i > 0 {
			buf.WriteString(",\n")
		}
		if a.StorageProfile == api.StorageAccount {
			buf.WriteString(fmt.Sprintf(dataDisks, diskSize, i, a.Name, i, a.Name, a.Name, a.Name, a.Name, i))
		} else if a.StorageProfile == api.ManagedDisks {
			buf.WriteString(fmt.Sprintf(managedDataDisks, diskSize, i))
		}
	}
	buf.WriteString("\n          ],")
	return buf.String()
}

func getSecurityRules(ports []int) string {
	var buf bytes.Buffer
	for index, port := range ports {
		if index > 0 {
			buf.WriteString(",\n")
		}
		buf.WriteString(getSecurityRule(port, index))
	}
	return buf.String()
}

// getSingleLine returns the file as a single line
func (t *TemplateGenerator) getSingleLine(textFilename string, cs *api.ContainerService, profile interface{}) (string, error) {
	b, err := Asset(textFilename)
	if err != nil {
		return "", t.Translator.Errorf("yaml file %s does not exist", textFilename)
	}

	// use go templates to process the text filename
	templ := template.New("customdata template").Funcs(t.getTemplateFuncMap(cs))
	if _, err = templ.New(textFilename).Parse(string(b)); err != nil {
		return "", t.Translator.Errorf("error parsing file %s: %v", textFilename, err)
	}

	var buffer bytes.Buffer
	if err = templ.ExecuteTemplate(&buffer, textFilename, profile); err != nil {
		return "", t.Translator.Errorf("error executing template for file %s: %v", textFilename, err)
	}
	expandedTemplate := buffer.String()

	return expandedTemplate, nil
}

// getSingleLineForTemplate returns the file as a single line for embedding in an arm template
func (t *TemplateGenerator) getSingleLineForTemplate(textFilename string, cs *api.ContainerService, profile interface{}) (string, error) {
	expandedTemplate, err := t.getSingleLine(textFilename, cs, profile)
	if err != nil {
		return "", err
	}

	textStr := escapeSingleLine(string(expandedTemplate))

	return textStr, nil
}

func escapeSingleLine(escapedStr string) string {
	// template.JSEscapeString leaves undesirable chars that don't work with pretty print
	escapedStr = strings.Replace(escapedStr, "\\", "\\\\", -1)
	escapedStr = strings.Replace(escapedStr, "\r\n", "\\n", -1)
	escapedStr = strings.Replace(escapedStr, "\n", "\\n", -1)
	escapedStr = strings.Replace(escapedStr, "\"", "\\\"", -1)
	return escapedStr
}

// getBase64CustomScript will return a base64 of the CSE
func getBase64CustomScript(csFilename string) string {
	b, err := Asset(csFilename)
	if err != nil {
		// this should never happen and this is a bug
		panic(fmt.Sprintf("BUG: %s", err.Error()))
	}
	// translate the parameters
	csStr := string(b)
	csStr = strings.Replace(csStr, "\r\n", "\n", -1)
	return getBase64CustomScriptFromStr(csStr)
}

// getBase64CustomScript will return a base64 of the CSE
func getBase64CustomScriptFromStr(str string) string {
	var gzipB bytes.Buffer
	w := gzip.NewWriter(&gzipB)
	w.Write([]byte(str))
	w.Close()
	return base64.StdEncoding.EncodeToString(gzipB.Bytes())
}

func getAddonFuncMap(addon api.KubernetesAddon) template.FuncMap {
	return template.FuncMap{
		"ContainerImage": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].Image
		},

		"ContainerCPUReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPURequests
		},

		"ContainerCPULimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].CPULimits
		},

		"ContainerMemReqs": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryRequests
		},

		"ContainerMemLimits": func(name string) string {
			i := addon.GetAddonContainersIndexByName(name)
			return addon.Containers[i].MemoryLimits
		},
		"ContainerConfig": func(name string) string {
			return addon.Config[name]
		},
	}
}

func getContainerAddonsString(properties *api.Properties, sourcePath string) string {
	var result string
	settingsMap := kubernetesContainerAddonSettingsInit(properties)

	var addonNames []string

	for addonName := range settingsMap {
		addonNames = append(addonNames, addonName)
	}

	sort.Strings(addonNames)

	for _, addonName := range addonNames {
		setting := settingsMap[addonName]
		if setting.isEnabled {
			var input string
			if setting.rawScript != "" {
				input = setting.rawScript
			} else {
				addon := properties.OrchestratorProfile.KubernetesConfig.GetAddonByName(addonName)
				templ := template.New("addon resolver template").Funcs(getAddonFuncMap(addon))
				addonFile := sourcePath + "/" + setting.sourceFile
				addonFileBytes, err := Asset(addonFile)
				if err != nil {
					return ""
				}
				_, err = templ.Parse(string(addonFileBytes))
				if err != nil {
					return ""
				}
				var buffer bytes.Buffer
				templ.Execute(&buffer, addon)
				input = buffer.String()
			}
			result += getAddonString(input, "/etc/kubernetes/addons", setting.destinationFile)
		}
	}
	return result
}

func getKubernetesSubnets(properties *api.Properties) string {
	subnetString := `{
            "name": "podCIDR%d",
            "properties": {
              "addressPrefix": "10.244.%d.0/24",
              "networkSecurityGroup": {
                "id": "[variables('nsgID')]"
              },
              "routeTable": {
                "id": "[variables('routeTableID')]"
              }
            }
          }`
	var buf bytes.Buffer

	cidrIndex := getKubernetesPodStartIndex(properties)
	for _, agentProfile := range properties.AgentPoolProfiles {
		if agentProfile.OSType == api.Windows {
			for i := 0; i < agentProfile.Count; i++ {
				buf.WriteString(",\n")
				buf.WriteString(fmt.Sprintf(subnetString, cidrIndex, cidrIndex))
				cidrIndex++
			}
		}
	}
	return buf.String()
}

func getKubernetesPodStartIndex(properties *api.Properties) int {
	nodeCount := 0
	nodeCount += properties.MasterProfile.Count
	for _, agentProfile := range properties.AgentPoolProfiles {
		if agentProfile.OSType != api.Windows {
			nodeCount += agentProfile.Count
		}
	}

	return nodeCount + 1
}

// getLinkedTemplatesForExtensions returns the
// Microsoft.Resources/deployments for each extension
//func getLinkedTemplatesForExtensions(properties api.Properties) string {
func getLinkedTemplatesForExtensions(properties *api.Properties) string {
	var result string

	extensions := properties.ExtensionProfiles
	masterProfileExtensions := properties.MasterProfile.Extensions
	orchestratorType := properties.OrchestratorProfile.OrchestratorType

	for err, extensionProfile := range extensions {
		_ = err

		masterOptedForExtension, singleOrAll := validateProfileOptedForExtension(extensionProfile.Name, masterProfileExtensions)
		if masterOptedForExtension {
			result += ","
			dta, e := getMasterLinkedTemplateText(properties.MasterProfile, orchestratorType, extensionProfile, singleOrAll)
			if e != nil {
				fmt.Println(e.Error())
				return ""
			}
			result += dta
		}

		for _, agentPoolProfile := range properties.AgentPoolProfiles {
			poolProfileExtensions := agentPoolProfile.Extensions
			poolOptedForExtension, singleOrAll := validateProfileOptedForExtension(extensionProfile.Name, poolProfileExtensions)
			if poolOptedForExtension {
				result += ","
				dta, e := getAgentPoolLinkedTemplateText(agentPoolProfile, orchestratorType, extensionProfile, singleOrAll)
				if e != nil {
					fmt.Println(e.Error())
					return ""
				}
				result += dta
			}

		}
	}

	return result
}

func getMasterLinkedTemplateText(masterProfile *api.MasterProfile, orchestratorType string, extensionProfile *api.ExtensionProfile, singleOrAll string) (string, error) {
	extTargetVMNamePrefix := "variables('masterVMNamePrefix')"

	// Due to upgrade k8s sometimes needs to install just some of the nodes.
	loopCount := "[sub(variables('masterCount'), variables('masterOffset'))]"
	loopOffset := "variables('masterOffset')"

	if strings.EqualFold(singleOrAll, "single") {
		loopCount = "1"
	}
	return internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount,
		loopOffset, extensionProfile)
}

func getAgentPoolLinkedTemplateText(agentPoolProfile *api.AgentPoolProfile, orchestratorType string, extensionProfile *api.ExtensionProfile, singleOrAll string) (string, error) {
	extTargetVMNamePrefix := fmt.Sprintf("variables('%sVMNamePrefix')", agentPoolProfile.Name)
	loopCount := fmt.Sprintf("[variables('%sCount'))]", agentPoolProfile.Name)
	loopOffset := ""

	// Availability sets can have an offset since we don't redeploy vms.
	// So we don't want to rerun these extensions in scale up scenarios.
	if agentPoolProfile.IsAvailabilitySets() {
		loopCount = fmt.Sprintf("[sub(variables('%sCount'), variables('%sOffset'))]",
			agentPoolProfile.Name, agentPoolProfile.Name)
		loopOffset = fmt.Sprintf("variables('%sOffset')", agentPoolProfile.Name)
	}

	if strings.EqualFold(singleOrAll, "single") {
		loopCount = "1"
	}

	return internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount,
		loopOffset, extensionProfile)
}

func internalGetPoolLinkedTemplateText(extTargetVMNamePrefix, orchestratorType, loopCount, loopOffset string, extensionProfile *api.ExtensionProfile) (string, error) {
	dta, e := getLinkedTemplateTextForURL(extensionProfile.RootURL, orchestratorType, extensionProfile.Name, extensionProfile.Version, extensionProfile.URLQuery)
	if e != nil {
		return "", e
	}
	if strings.Contains(extTargetVMNamePrefix, "master") {
		dta = strings.Replace(dta, "EXTENSION_TARGET_VM_TYPE", "master", -1)
	} else {
		dta = strings.Replace(dta, "EXTENSION_TARGET_VM_TYPE", "agent", -1)
	}
	extensionsParameterReference := fmt.Sprintf("[parameters('%sParameters')]", extensionProfile.Name)
	dta = strings.Replace(dta, "EXTENSION_PARAMETERS_REPLACE", extensionsParameterReference, -1)
	dta = strings.Replace(dta, "EXTENSION_URL_REPLACE", extensionProfile.RootURL, -1)
	dta = strings.Replace(dta, "EXTENSION_TARGET_VM_NAME_PREFIX", extTargetVMNamePrefix, -1)
	if _, err := strconv.Atoi(loopCount); err == nil {
		dta = strings.Replace(dta, "\"EXTENSION_LOOP_COUNT\"", loopCount, -1)
	} else {
		dta = strings.Replace(dta, "EXTENSION_LOOP_COUNT", loopCount, -1)
	}

	dta = strings.Replace(dta, "EXTENSION_LOOP_OFFSET", loopOffset, -1)
	return dta, nil
}

func validateProfileOptedForExtension(extensionName string, profileExtensions []api.Extension) (bool, string) {
	for _, extension := range profileExtensions {
		if extensionName == extension.Name {
			return true, extension.SingleOrAll
		}
	}
	return false, ""
}

// getLinkedTemplateTextForURL returns the string data from
// template-link.json in the following directory:
// extensionsRootURL/extensions/extensionName/version
// It returns an error if the extension cannot be found
// or loaded.  getLinkedTemplateTextForURL provides the ability
// to pass a root extensions url for testing
func getLinkedTemplateTextForURL(rootURL, orchestrator, extensionName, version, query string) (string, error) {
	supportsExtension, err := orchestratorSupportsExtension(rootURL, orchestrator, extensionName, version, query)
	if !supportsExtension {
		return "", errors.Wrap(err, "Extension not supported for orchestrator")
	}

	templateLinkBytes, err := getExtensionResource(rootURL, extensionName, version, "template-link.json", query)
	if err != nil {
		return "", err
	}

	return string(templateLinkBytes), nil
}

func orchestratorSupportsExtension(rootURL, orchestrator, extensionName, version, query string) (bool, error) {
	orchestratorBytes, err := getExtensionResource(rootURL, extensionName, version, "supported-orchestrators.json", query)
	if err != nil {
		return false, err
	}

	var supportedOrchestrators []string
	err = json.Unmarshal(orchestratorBytes, &supportedOrchestrators)
	if err != nil {
		return false, errors.Errorf("Unable to parse supported-orchestrators.json for Extension %s Version %s", extensionName, version)
	}

	if !stringInSlice(orchestrator, supportedOrchestrators) {
		return false, errors.Errorf("Orchestrator: %s not in list of supported orchestrators for Extension: %s Version %s", orchestrator, extensionName, version)
	}

	return true, nil
}

func getExtensionResource(rootURL, extensionName, version, fileName, query string) ([]byte, error) {
	requestURL := getExtensionURL(rootURL, extensionName, version, fileName, query)

	res, err := http.Get(requestURL)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to GET extension resource for extension: %s with version %s with filename %s at URL: %s", extensionName, version, fileName, requestURL)
	}

	defer res.Body.Close()

	if res.StatusCode != 200 {
		return nil, errors.Errorf("Unable to GET extension resource for extension: %s with version %s with filename %s at URL: %s StatusCode: %s: Status: %s", extensionName, version, fileName, requestURL, strconv.Itoa(res.StatusCode), res.Status)
	}

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to GET extension resource for extension: %s with version %s  with filename %s at URL: %s", extensionName, version, fileName, requestURL)
	}

	return body, nil
}

func getExtensionURL(rootURL, extensionName, version, fileName, query string) string {
	extensionsDir := "extensions"
	url := rootURL + extensionsDir + "/" + extensionName + "/" + version + "/" + fileName
	if query != "" {
		url += "?" + query
	}
	return url
}

func stringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}
