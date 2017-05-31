/*
 * Minio Cloud Storage, (C) 2017 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	router "github.com/gorilla/mux"
)

const (
	// STS API version.
	stsAPIVersion = "2011-06-15"
)

// stsAPIHandlers implements and provides http handlers for AWS STS API.
type stsAPIHandlers struct{}

// registerSTSRouter - registers AWS STS compatible APIs.
func registerSTSRouter(mux *router.Router) {
	// Initialize STS.
	sts := &stsAPIHandlers{}

	// STS Router
	stsRouter := mux.NewRoute().PathPrefix("/").Subrouter()

	// AssumeRoleWithSAML
	stsRouter.Methods("POST").HandlerFunc(sts.AssumeRoleWithSAMLHandler)
}

// AssumedRoleUser - The identifiers for the temporary security credentials that
// the operation returns. Please also see https://docs.aws.amazon.com/goto/WebAPI/sts-2011-06-15/AssumedRoleUser
type AssumedRoleUser struct {
	// The ARN of the temporary security credentials that are returned from the
	// AssumeRole action. For more information about ARNs and how to use them in
	// policies, see IAM Identifiers (http://docs.aws.amazon.com/IAM/latest/UserGuide/reference_identifiers.html)
	// in Using IAM.
	//
	// Arn is a required field
	Arn string

	// A unique identifier that contains the role ID and the role session name of
	// the role that is being assumed. The role ID is generated by AWS when the
	// role is created.
	//
	// AssumedRoleId is a required field
	AssumedRoleID string `xml:"AssumeRoleId"`
	// contains filtered or unexported fields
}

// AssumeRoleWithSAMLResult - Contains the response to a successful AssumeRoleWithSAML request,
// including temporary AWS credentials that can be used to make AWS requests.
// Please also see https://docs.aws.amazon.com/goto/WebAPI/sts-2011-06-15/AssumeRoleWithSAMLResponse
type AssumeRoleWithSAMLResult struct {
	// The identifiers for the temporary security credentials that the operation
	// returns.
	AssumedRoleUser AssumedRoleUser `xml:",omitempty"`

	// The value of the Recipient attribute of the SubjectConfirmationData element
	// of the SAML assertion.
	Audience string `xml:",omitempty"`

	// The temporary security credentials, which include an access key ID, a secret
	// access key, and a security (or session) token.
	//
	// Note: The size of the security token that STS APIs return is not fixed. We
	// strongly recommend that you make no assumptions about the maximum size. As
	// of this writing, the typical size is less than 4096 bytes, but that can vary.
	// Also, future updates to AWS might require larger sizes.
	Credentials credential `xml:",omitempty"`

	// The value of the Issuer element of the SAML assertion.
	Issuer string `xml:",omitempty"`

	// A hash value based on the concatenation of the Issuer response value, the
	// AWS account ID, and the friendly name (the last part of the ARN) of the SAML
	// provider in IAM. The combination of NameQualifier and Subject can be used
	// to uniquely identify a federated user.
	//
	// The following pseudocode shows how the hash value is calculated:
	//
	// BASE64 ( SHA1 ( "https://example.com/saml" + "123456789012" + "/MySAMLIdP"
	// ) )
	NameQualifier string `xml:",omitempty"`

	// A percentage value that indicates the size of the policy in packed form.
	// The service rejects any policy with a packed size greater than 100 percent,
	// which means the policy exceeded the allowed space.
	PackedPolicySize int64 `xml:",omitempty"`

	// The value of the NameID element in the Subject element of the SAML assertion.
	Subject string `xml:",omitempty"`

	// The format of the name ID, as defined by the Format attribute in the NameID
	// element of the SAML assertion. Typical examples of the format are transient
	// or persistent.
	//
	// If the format includes the prefix urn:oasis:names:tc:SAML:2.0:nameid-format,
	// that prefix is removed. For example, urn:oasis:names:tc:SAML:2.0:nameid-format:transient
	// is returned as transient. If the format includes any other prefix, the format
	// is returned with no modifications.
	SubjectType string `xml:",omitempty"`
}

func (sts *stsAPIHandlers) AssumeRoleWithSAMLHandler(w http.ResponseWriter, r *http.Request) {
	// This is an unauthenticated request.
	if err := r.ParseForm(); err != nil {
		errorIf(err, "Unable to parse incoming data.")
		writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
		return
	}

	if r.PostForm.Get("Version") != stsAPIVersion {
		errorIf(errors.New("API version mismatch"), "")
		writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
		return
	}

	samlResp, err := ParseSAMLResponse(r.PostForm.Get("SAMLAssertion"))
	if err != nil {
		errorIf(err, "Unable to parse saml assertion.")
		writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
		return
	}

	// Keep TLS config.
	tlsConfig := &tls.Config{
		RootCAs:            globalRootCAs,
		InsecureSkipVerify: true,
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			TLSClientConfig:       tlsConfig,
		},
	}

	resp, rerr := client.PostForm(samlResp.Destination, url.Values{
		"SAMLResponse": {samlResp.origSAMLAssertion},
	})
	if rerr != nil {
		errorIf(rerr, "Unable to validate saml assertion.")
		writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
		return
	}

	if resp.StatusCode >= http.StatusInternalServerError {
		errorIf(errors.New(resp.Status), "Unable to validate saml assertion.")
		writeSTSErrorResponse(w, ErrSTSIDPRejectedClaim)
		return
	}

	expiryTime := UTCNow().Add(time.Duration(240) * time.Minute) // Defaults to 4hrs.
	if r.PostForm.Get("DurationSeconds") != "" {
		expirySecs, serr := strconv.ParseInt(r.PostForm.Get("DurationSeconds"), 10, 64)
		if serr != nil {
			errorIf(serr, "Unable to parse DurationSeconds")
			writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
			return
		}

		// The duration, in seconds, of the role session.
		// The value can range from 900 seconds (15 minutes)
		// to 14400 seconds (4 hours). By default, the value
		// is set to 14400 seconds.
		if expirySecs < 900 {
			expirySecs = 900
		}

		if expirySecs > 14400 {
			expirySecs = 14400
		}

		expiryTime = UTCNow().Add(time.Duration(expirySecs) * time.Second)
	}

	cred, err := getNewCredentialWithExpiry(expiryTime)
	if err != nil {
		errorIf(err, "Failed to general new credentials with expiry.")
		writeSTSErrorResponse(w, ErrSTSMalformedPolicyDocument)
		return
	}

	h := sha1.New()
	io.WriteString(h, samlResp.Issuer.URL+"0000"+"myidp")
	nq := base64.StdEncoding.EncodeToString(h.Sum(nil))

	// Set the newly generated credentials.
	globalServerCreds.SetCredential(cred)

	samlOutput := &AssumeRoleWithSAMLResult{
		Credentials: cred,
		// TODO
		// Subject:       samlResp.Assertion.Subject.NameID.Value,
		// SubjectType:   samlResp.Assertion.Subject.NameID.Format,
		Issuer:        samlResp.Issuer.URL,
		NameQualifier: nq,
	}

	encodedSuccessResponse := encodeResponse(samlOutput)
	writeSuccessResponseXML(w, encodedSuccessResponse)
}