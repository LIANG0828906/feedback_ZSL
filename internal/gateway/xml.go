package gateway

import (
	"encoding/xml"
	"strconv"
)

const s3XMLNS = "http://s3.amazonaws.com/doc/2006-03-01/"

type initiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr,omitempty"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []completePart `xml:"Part"`
}

type completePart struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

type completeMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr,omitempty"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

type listPartsResult struct {
	XMLName              xml.Name       `xml:"ListPartsResult"`
	XMLNS                string         `xml:"xmlns,attr,omitempty"`
	Bucket               string         `xml:"Bucket"`
	Key                  string         `xml:"Key"`
	UploadID             string         `xml:"UploadId"`
	PartNumberMarker     int            `xml:"PartNumberMarker"`
	NextPartNumberMarker int            `xml:"NextPartNumberMarker"`
	MaxParts             int            `xml:"MaxParts"`
	IsTruncated          bool           `xml:"IsTruncated"`
	Part                 []listPartItem `xml:"Part"`
}

type listPartItem struct {
	PartNumber   int    `xml:"PartNumber"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
}

type listMultipartUploadsResult struct {
	XMLName            xml.Name              `xml:"ListMultipartUploadsResult"`
	XMLNS              string                `xml:"xmlns,attr,omitempty"`
	Bucket             string                `xml:"Bucket"`
	KeyMarker          string                `xml:"KeyMarker"`
	UploadIDMarker     string                `xml:"UploadIdMarker"`
	NextKeyMarker      string                `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string                `xml:"NextUploadIdMarker,omitempty"`
	Delimiter          string                `xml:"Delimiter,omitempty"`
	Prefix             string                `xml:"Prefix"`
	EncodingType       string                `xml:"EncodingType,omitempty"`
	MaxUploads         int                   `xml:"MaxUploads"`
	IsTruncated        bool                  `xml:"IsTruncated"`
	Uploads            []multipartUploadItem `xml:"Upload"`
}

type multipartUploadItem struct {
	Key          string `xml:"Key"`
	UploadID     string `xml:"UploadId"`
	StorageClass string `xml:"StorageClass"`
	Initiated    string `xml:"Initiated"`
}

type listVersionsResult struct {
	XMLName             xml.Name
	XMLNS               string
	Name                string
	Prefix              string
	KeyMarker           string
	VersionIDMarker     string
	NextKeyMarker       string
	NextVersionIDMarker string
	MaxKeys             int
	IsTruncated         bool
	Entries             []versionListEntry
}

type versionListEntry struct {
	Version *listVersionItem
	Marker  *listDeleteMarker
}

func (r listVersionsResult) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{Local: "ListVersionsResult"}
	start.Attr = []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: s3XMLNS}}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"Name", r.Name},
		{"Prefix", r.Prefix},
		{"KeyMarker", r.KeyMarker},
		{"VersionIdMarker", r.VersionIDMarker},
		{"NextKeyMarker", r.NextKeyMarker},
		{"NextVersionIdMarker", r.NextVersionIDMarker},
		{"MaxKeys", strconv.Itoa(r.MaxKeys)},
		{"IsTruncated", strconv.FormatBool(r.IsTruncated)},
	} {
		if field.name == "NextKeyMarker" && field.value == "" {
			continue
		}
		if field.name == "NextVersionIdMarker" && field.value == "" {
			continue
		}
		if err := e.EncodeElement(field.value, xml.StartElement{Name: xml.Name{Local: field.name}}); err != nil {
			return err
		}
	}
	for _, entry := range r.Entries {
		if entry.Version != nil {
			if err := e.EncodeElement(*entry.Version, xml.StartElement{Name: xml.Name{Local: "Version"}}); err != nil {
				return err
			}
		}
		if entry.Marker != nil {
			if err := e.EncodeElement(*entry.Marker, xml.StartElement{Name: xml.Name{Local: "DeleteMarker"}}); err != nil {
				return err
			}
		}
	}
	return e.EncodeToken(start.End())
}

type listVersionItem struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type listDeleteMarker struct {
	Key          string `xml:"Key"`
	VersionID    string `xml:"VersionId"`
	IsLatest     bool   `xml:"IsLatest"`
	LastModified string `xml:"LastModified"`
}

type listBucketResult struct {
	XMLName     xml.Name          `xml:"ListBucketResult"`
	XMLNS       string            `xml:"xmlns,attr,omitempty"`
	Name        string            `xml:"Name"`
	Prefix      string            `xml:"Prefix"`
	StartAfter  string            `xml:"StartAfter,omitempty"`
	KeyCount    int               `xml:"KeyCount"`
	MaxKeys     int               `xml:"MaxKeys"`
	IsTruncated bool              `xml:"IsTruncated"`
	Contents    []listObjectEntry `xml:"Contents"`
}

type listObjectEntry struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"`
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`
}

type listAllMyBucketsResult struct {
	XMLName xml.Name        `xml:"ListAllMyBucketsResult"`
	XMLNS   string          `xml:"xmlns,attr,omitempty"`
	Buckets []bucketXMLItem `xml:"Buckets>Bucket"`
}

type bucketXMLItem struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"`
}

type versioningConfiguration struct {
	XMLName xml.Name `xml:"VersioningConfiguration"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status"`
}

type locationConstraint struct {
	XMLName xml.Name `xml:"LocationConstraint"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Region  string   `xml:",chardata"`
}
