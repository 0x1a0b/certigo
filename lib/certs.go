/*-
 * Copyright 2016 Square Inc.
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

package lib

import (
	"bufio"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/square/certigo/jceks"
	"github.com/square/certigo/pkcs7"
	"golang.org/x/crypto/pkcs12"
)

const (
	// nameHeader is the PEM header field for the friendly name/alias of the key in the key store.
	nameHeader = "friendlyName"

	// fileHeader is the origin file where the key came from (as in file on disk).
	fileHeader = "originFile"
)

var fileExtToFormat = map[string]string{
	".pem":   "PEM",
	".crt":   "PEM",
	".p7b":   "PEM",
	".p7c":   "PEM",
	".p12":   "PKCS12",
	".pfx":   "PKCS12",
	".jceks": "JCEKS",
	".jks":   "JCEKS", // Only partially supported
	".der":   "DER",
}

var badSignatureAlgorithms = [...]x509.SignatureAlgorithm{
	x509.MD2WithRSA,
	x509.MD5WithRSA,
	x509.SHA1WithRSA,
	x509.DSAWithSHA1,
	x509.ECDSAWithSHA1,
}

// ReadPEMFromFiles will read PEM blocks from the given set of inputs.
func ReadPEMFromFiles(files []*os.File, format string, password func(string) string, callback func(*pem.Block)) error {
	for _, file := range files {
		reader := bufio.NewReaderSize(file, 4)
		format, err := formatForFile(reader, file.Name(), format)
		if err != nil {
			return fmt.Errorf("unable to guess file type (for file %s)\n", file.Name())
		}

		readCertsFromStream(reader, file.Name(), format, password, callback)
	}
	return nil
}

// ReadPEM will read PEM blocks from the given set of inputs.
func ReadPEM(readers []io.Reader, format string, password func(string) string, callback func(*pem.Block)) error {
	for _, r := range readers {
		reader := bufio.NewReaderSize(r, 4)
		format, err := formatForFile(reader, "", format)
		if err != nil {
			return fmt.Errorf("unable to guess format for input stream")
		}

		readCertsFromStream(reader, "", format, password, callback)
	}
	return nil
}

// ReadX509FromFiles will read X.509 certificates from the given set of inputs.
func ReadX509FromFiles(files []*os.File, format string, password func(string) string, callback func(*x509.Certificate)) error {
	for _, file := range files {
		reader := bufio.NewReaderSize(file, 4)
		format, err := formatForFile(reader, file.Name(), format)
		if err != nil {
			return fmt.Errorf("unable to guess file type (for file %s)", file.Name())
		}

		readCertsFromStream(reader, file.Name(), format, password, pemToX509(callback))
	}

	return nil
}

// ReadX509 will read X.509 certificates from the given set of inputs.
func ReadX509(readers []io.Reader, format string, password func(string) string, callback func(*x509.Certificate)) error {
	for _, r := range readers {
		reader := bufio.NewReaderSize(r, 4)
		format, err := formatForFile(reader, "", format)
		if err != nil {
			return fmt.Errorf("unable to guess format for input stream")
		}

		readCertsFromStream(reader, "", format, password, pemToX509(callback))
	}

	return nil
}

func pemToX509(callback func(*x509.Certificate)) func(*pem.Block) {
	return func(block *pem.Block) {
		switch block.Type {
		case "CERTIFICATE":
			cert, err := x509.ParseCertificate(block.Bytes)
			if err == nil {
				callback(cert)
			}
		case "PKCS7":
			certs, err := pkcs7.ExtractCertificates(block.Bytes)
			if err == nil {
				for _, cert := range certs {
					callback(cert)
				}
			}
		}
	}
}

// readCertsFromStream takes some input and converts it to PEM blocks.
func readCertsFromStream(reader io.Reader, filename string, format string, password func(string) string, callback func(*pem.Block)) error {
	headers := map[string]string{}
	if filename != "" && filename != os.Stdin.Name() {
		headers[fileHeader] = filename
	}

	switch format {
	case "PEM":
		scanner := pemScanner(reader)
		for scanner.Scan() {
			block, _ := pem.Decode(scanner.Bytes())
			block.Headers = mergeHeaders(block.Headers, headers)
			callback(block)
		}
	case "DER":
		data, err := ioutil.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("error reading input: %s\n", err)
		}
		x509Certs, err := x509.ParseCertificates(data)
		if err == nil {
			for _, cert := range x509Certs {
				callback(EncodeX509ToPEM(cert, headers))
			}
			return nil
		}
		p7bBlocks, err := pkcs7.ParseSignedData(data)
		if err == nil {
			for _, block := range p7bBlocks {
				callback(pkcs7ToPem(block, headers))
			}
			return nil
		}
		return fmt.Errorf("error parsing certificates from DER data\n")
	case "PKCS12":
		data, err := ioutil.ReadAll(reader)
		if err != nil {
			return fmt.Errorf("error reading input: %s\n", err)
		}
		blocks, err := pkcs12.ToPEM(data, password(""))
		if err != nil || len(blocks) == 0 {
			fmt.Fprint(os.Stderr, "keystore appears to be empty or password was incorrect\n")
		}
		for _, block := range blocks {
			block.Headers = mergeHeaders(block.Headers, headers)
			callback(block)
		}
	case "JCEKS":
		keyStore, err := jceks.LoadFromReader(reader, []byte(password("")))
		if err != nil {
			return fmt.Errorf("error parsing keystore: %s\n", err)
		}
		for _, alias := range keyStore.ListCerts() {
			cert, _ := keyStore.GetCert(alias)
			callback(EncodeX509ToPEM(cert, mergeHeaders(headers, map[string]string{nameHeader: alias})))
		}
		for _, alias := range keyStore.ListPrivateKeys() {
			key, certs, err := keyStore.GetPrivateKeyAndCerts(alias, []byte(password(alias)))
			if err != nil {
				return fmt.Errorf("error parsing keystore: %s\n", err)
			}
			block, err := keyToPem(key, mergeHeaders(headers, map[string]string{nameHeader: alias}))
			if err != nil {
				return fmt.Errorf("error reading key: %s\n", err)
			}
			callback(block)
			for _, cert := range certs {
				callback(EncodeX509ToPEM(cert, mergeHeaders(headers, map[string]string{nameHeader: alias})))
			}
		}
	}
	return fmt.Errorf("unknown file type: %s\n", format)
}

func mergeHeaders(baseHeaders, extraHeaders map[string]string) (headers map[string]string) {
	headers = map[string]string{}
	for k, v := range baseHeaders {
		headers[k] = v
	}
	for k, v := range extraHeaders {
		headers[k] = v
	}
	return
}

// EncodeX509ToPEM converts an X.509 certificate into a PEM block for output.
func EncodeX509ToPEM(cert *x509.Certificate, headers map[string]string) *pem.Block {
	return &pem.Block{
		Type:    "CERTIFICATE",
		Bytes:   cert.Raw,
		Headers: headers,
	}
}

// Convert a PKCS7 envelope into a PEM block for output.
func pkcs7ToPem(block *pkcs7.SignedDataEnvelope, headers map[string]string) *pem.Block {
	return &pem.Block{
		Type:    "PKCS7",
		Bytes:   block.Raw,
		Headers: headers,
	}
}

// Convert a key into one or more PEM blocks for output.
func keyToPem(key crypto.PrivateKey, headers map[string]string) (*pem.Block, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return &pem.Block{
			Type:    "RSA PRIVATE KEY",
			Bytes:   x509.MarshalPKCS1PrivateKey(k),
			Headers: headers,
		}, nil
	case *ecdsa.PrivateKey:
		raw, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, fmt.Errorf("error marshaling key: %s\n", reflect.TypeOf(key))
		}
		return &pem.Block{
			Type:    "EC PRIVATE KEY",
			Bytes:   raw,
			Headers: headers,
		}, nil
	}
	return nil, fmt.Errorf("unknown key type: %s\n", reflect.TypeOf(key))
}

// formatForFile returns the file format (either from flags or
// based on file extension).
func formatForFile(file *bufio.Reader, filename, format string) (string, error) {
	// First, honor --format flag we got from user
	if format != "" {
		return format, nil
	}

	// Second, attempt to guess based on extension
	guess, ok := fileExtToFormat[strings.ToLower(filepath.Ext(filename))]
	if ok {
		return guess, nil
	}

	// Third, attempt to guess based on first 4 bytes of input
	data, err := file.Peek(4)
	if err != nil {
		return "", fmt.Errorf("unable to read file: %s\n", err)
	}

	// Heuristics for guessing -- best effort.
	magic := binary.BigEndian.Uint32(data)
	if magic == 0xCECECECE || magic == 0xFEEDFEED {
		// JCEKS/JKS files always start with this prefix
		return "JCEKS", nil
	}
	if magic == 0x2D2D2D2D || magic == 0x434f4e4e {
		// Starts with '----' or 'CONN' (what s_client prints...)
		return "PEM", nil
	}
	if magic&0xFFFF0000 == 0x30820000 {
		// Looks like the input is DER-encoded, so it's either PKCS12 or X.509.
		if magic&0x0000FF00 == 0x0300 {
			// Probably X.509
			return "DER", nil
		}
		return "PKCS12", nil
	}

	return "", fmt.Errorf("unable to guess file format")
}

// pemScanner will return a bufio.Scanner that splits the input
// from the given reader into PEM blocks.
func pemScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)

	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		block, rest := pem.Decode(data)
		if block != nil {
			size := len(data) - len(rest)
			return size, data[:size], nil
		}

		return 0, nil, nil
	})

	return scanner
}
