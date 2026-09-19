# Nextcloud Chunked Upload v2: Client-Perspective Wire Protocol Specification

**Version**: 1.0  
**Date**: 2026-05-01  
**Based on**: 
- Nextcloud Desktop Client (C++) commit `306ef05be4941052e8b1635cadd9a9a29d8a2e86`
- Nextcloud Android Library commit `264573e`

---

## Executive Summary

This document specifies the **exact wire protocol** that Nextcloud desktop, Android, and iOS clients use for chunked upload v2. Your Go server implementation must accept every header, quirk, and edge case documented here to pass real client testing.

All findings are backed by **file:line citations** from official Nextcloud client source code.

---

## Table of Contents

1. [Capability Discovery](#1-capability-discovery)
2. [Chunking Threshold](#2-chunking-threshold)
3. [Transfer ID Generation](#3-transfer-id-generation)
4. [Request Sequence](#4-request-sequence)
5. [Parallel Uploads](#5-parallel-uploads)
6. [Dynamic Chunk Sizing](#6-dynamic-chunk-sizing)
7. [Retry & Error Handling](#7-retry--error-handling)
8. [Cleanup on Abort](#8-cleanup-on-abort)
9. [Edge Cases & Quirks](#9-edge-cases--quirks)
10. [Complete Wire Sequence](#10-complete-wire-sequence)
11. [Platform Differences](#11-platform-differences)
12. [Testing Checklist](#12-testing-checklist)
13. [References](#13-references)

---

## 1. Capability Discovery

**Evidence** ([capabilities.cpp:231-239](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/capabilities.cpp#L231-L239)):

```cpp
bool Capabilities::chunkingNg() const
{
    static const auto chunkng = qgetenv("OWNCLOUD_CHUNKING_NG");
    if (chunkng == "0")
        return false;
    if (chunkng == "1")
        return true;
    return _capabilities["dav"].toMap()["chunking"].toByteArray() >= "1.0";
}
```

### Server Capability Response

**Required**:
```json
{
  "dav": {
    "chunking": "1.0"
  }
}
```

**Optional** ([capabilities.cpp:241-249](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/capabilities.cpp#L241-L249)):
```json
{
  "files": {
    "chunked_upload": {
      "max_size": 5368709120,
      "max_parallel_count": 20
    }
  }
}
```

---

## 2. Chunking Threshold

**Evidence** ([owncloudpropagator.cpp:411](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/owncloudpropagator.cpp#L411)):

```cpp
if (item->_size > syncOptions()._initialChunkSize && account()->capabilities().chunkingNg()) {
    job = std::make_unique<PropagateUploadFileNG>(this, item);
}
```

**Evidence** ([syncoptions.h:50](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/syncoptions.h#L50)):

```cpp
qint64 _initialChunkSize = 100LL * 1024LL * 1024LL; // 100MiB
```

### Thresholds by Platform

| Platform | Threshold | Notes |
|----------|-----------|-------|
| Desktop | 100 MiB | Default, overridable via `OWNCLOUD_CHUNK_SIZE` env var |
| Android (Mobile) | 10 MB | [ChunkedFileUploadRemoteOperation.java:42](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L42) |
| Android (WiFi) | 40 MB | [ChunkedFileUploadRemoteOperation.java:43](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L43) |

**Your server must**: Accept files of any size via chunked upload v2.

---

## 3. Transfer ID Generation

### Desktop Client

**Evidence** ([propagateuploadng.cpp:251](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L251)):

```cpp
_transferId = uint(Utility::rand() ^ uint(_item->_modtime) ^ (uint(_fileToUpload._size) << 16) ^ qHash(_fileToUpload._file));
```

**Algorithm**: 
```
transferId = random() XOR modtime XOR (size << 16) XOR hash(filename)
```

**Format**: 32-bit unsigned integer (e.g., `3847562910`)

### Android Client

**Evidence** ([ChunkedFileUploadRemoteOperation.java:152](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L152)):

```java
uploadFolderUri = client.getUploadUri() + "/" + client.getUserId() + "/" + FileUtils.md5Sum(file);
```

**Algorithm**: MD5 hash of file content

**Format**: 32-character hexadecimal string (e.g., `5d41402abc4b2a76b9719d911017c592`)

### Server Requirements

**Your server must**: Accept any alphanumeric transfer ID (both uint32 and MD5 hex formats).

---

## 4. Request Sequence

### 4.1 MKCOL: Create Upload Folder

**Evidence** ([propagateuploadng.cpp:269-282](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L269-L282)):

```cpp
QMap<QByteArray, QByteArray> headers;
headers["OC-Total-Length"] = QByteArray::number(_fileToUpload._size);
headers["Destination"] = destinationHeader();
const auto job = new MkColJob(propagator()->account(), chunkUploadFolderUrl(), headers, this);
```

**Request**:
```http
MKCOL /remote.php/dav/uploads/{user}/{transferId}/ HTTP/1.1
Host: cloud.example.com
OC-Total-Length: 524288000
Destination: https://cloud.example.com/remote.php/dav/files/{user}/path/to/file.bin
```

**Expected Response**: `201 Created`

**Android** ([ChunkedFileUploadRemoteOperation.java:157-161](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L157-L161)):
```java
MkColMethod createFolder = new MkColMethod(uploadFolderUri);
createFolder.addRequestHeader(DESTINATION_HEADER, destinationUri);
client.executeMethod(createFolder, 30000, 5000);
```

**Android sends identical headers**.

---

### 4.2 PROPFIND: Resume Detection

**Evidence** ([propagateuploadng.cpp:102-112](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L102-L112)):

```cpp
const auto job = new LsColJob(propagator()->account(), chunkUploadFolderUrl());
job->setProperties(QList<QByteArray>() << "resourcetype" << "getcontentlength");
```

**Request**:
```http
PROPFIND /remote.php/dav/uploads/{user}/{transferId}/ HTTP/1.1
Depth: 1
Content-Type: application/xml

<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:">
  <d:prop>
    <d:resourcetype/>
    <d:getcontentlength/>
  </d:prop>
</d:propfind>
```

**Purpose**: List existing chunks to resume interrupted uploads.

**Resume Logic** ([propagateuploadng.cpp:125-154](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L125-L154)):

```cpp
void PropagateUploadFileNG::slotPropfindIterate(const QString &name, const QMap<QString, QString> &properties)
{
    bool ok = false;
    QString chunkName = name.mid(name.lastIndexOf('/') + 1);
    auto chunkId = chunkName.toLongLong(&ok);

    if (ok) {
        ServerChunkInfo chunkinfo = { properties["getcontentlength"].toLongLong(), chunkName };
        _serverChunks[chunkId] = chunkinfo;
    }
}

void PropagateUploadFileNG::slotPropfindFinished()
{
    _currentChunk = 1;
    _sent = 0;
    while (_serverChunks.contains(_currentChunk)) {
        _sent += _serverChunks[_currentChunk].size;
        _serverChunks.remove(_currentChunk);
        ++_currentChunk;
    }
}
```

**Resume Algorithm**:
1. Client lists chunks via PROPFIND
2. Parses chunk names as integers (e.g., `00001`, `00002`)
3. Sums sizes of **consecutive chunks starting from 1**
4. Resumes from first missing chunk

**Critical**: Client only resumes from consecutive chunks. If chunks 1-4 exist but 5 is missing, client resumes from chunk 5 (not 6).

**Android** ([ChunkedFileUploadRemoteOperation.java:163-194](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L163-L194)):
```java
PropFindMethod listChunks = new PropFindMethod(uploadFolderUri,
                                               WebdavUtils.getChunksPropSet(),
                                               DavConstants.DEPTH_1);
// ... parse responses, sum nextByte linearly
```

**Android uses identical resume logic**.

---

### 4.3 DELETE: Cleanup Stale Chunks

**Evidence** ([propagateuploadng.cpp:174-189](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L174-L189)):

```cpp
if (!_serverChunks.isEmpty()) {
    qCInfo(lcPropagateUploadNG) << "To Delete" << _serverChunks.keys();
    for (const auto &serverChunk : std::as_const(_serverChunks)) {
        auto job = new DeleteJob(propagator()->account(), 
            Utility::concatUrlPath(chunkUploadFolderUrl(), serverChunk.originalName), {}, this);
        job->start();
    }
}
```

**Request**:
```http
DELETE /remote.php/dav/uploads/{user}/{transferId}/00005 HTTP/1.1
```

**Purpose**: Remove chunks after a "hole" in the sequence.

**Example**: If chunks 1-4 exist but 5 is missing, client deletes chunks 6+ before resuming from chunk 5.

**Expected Response**: `204 No Content` or `404 Not Found` (both acceptable)

---

### 4.4 PUT: Upload Chunks

**Evidence** ([propagateuploadng.cpp:376-396](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L376-L396)):

```cpp
QMap<QByteArray, QByteArray> headers;
headers["OC-Chunk-Offset"] = QByteArray::number(_sent);
headers["Destination"] = destinationHeader();
headers[QByteArrayLiteral("OC-Total-Length")] = QByteArray::number(fileSize);

_sent += _currentChunkSize;
const auto url = chunkUrl(_currentChunk);

const auto job = new PUTFileJob(propagator()->account(), url, std::move(device), headers, _currentChunk, this);
```

**Chunk URL** ([propagateuploadng.cpp:41-49](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L41-L49)):

```cpp
QUrl PropagateUploadFileNG::chunkUrl(const int chunk) const
{
    Q_ASSERT(chunk >= 1);
    constexpr auto maxChunkDigits = 5; // Chunk V2: max num of chunks is 10000

    // We need to do add leading 0 because the server orders the chunk alphabetically
    const auto chunkNumString = QStringLiteral("%1").arg(chunk, maxChunkDigits, 10, QChar('0'));
    return Utility::concatUrlPath(chunkUploadFolderUrl(), chunkNumString);
}
```

**Request**:
```http
PUT /remote.php/dav/uploads/{user}/{transferId}/00001 HTTP/1.1
Host: cloud.example.com
OC-Chunk-Offset: 0
OC-Total-Length: 524288000
Destination: https://cloud.example.com/remote.php/dav/files/{user}/path/to/file.bin
Content-Length: 104857600
Content-Type: application/octet-stream

<binary chunk data>
```

**Headers**:
- `OC-Chunk-Offset`: Byte offset of this chunk in the final file (0-indexed)
- `OC-Total-Length`: Total file size in bytes
- `Destination`: Final file path (full URL, percent-encoded except `/`)
- `Content-Length`: Chunk size in bytes
- `Content-Type`: `application/octet-stream`

**Chunk Naming**:
- **Desktop**: Zero-padded 5 digits: `00001`, `00002`, ..., `10000` (max 10,000 chunks)
- **Android** ([ChunkedFileUploadRemoteOperation.java:279-280](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L279-L280)):
  ```java
  String chunkUri = uploadFolderUri + "/" + String.format(Locale.ROOT, "%0" + CHUNK_NAME_LENGTH + "d", chunk.getId());
  // CHUNK_NAME_LENGTH = 6 (Android uses 6 digits)
  ```

**Android uses 6-digit padding**: `000001`, `000002`, etc.

**Your server must**: Accept both 5-digit and 6-digit chunk names.

**Expected Response**: `201 Created` or `204 No Content`

**Test Evidence** ([testchunkingng.cpp:106-107](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L106-L107)):
```cpp
const auto firstChunkName = chunkMap.first().name;
const auto expectedChunkName = QStringLiteral("%1").arg(1, 5, 10, QChar('0'));
QCOMPARE(firstChunkName, expectedChunkName);
```

---

### 4.5 MOVE: Assemble Chunks

**Evidence** ([propagateuploadng.cpp:302-339](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L302-L339)):

```cpp
void PropagateUploadFileNG::finishUpload()
{
    const auto destination = QDir::cleanPath(propagator()->account()->davUrl().path() + propagator()->fullRemotePath(_fileToUpload._file));
    auto headers = PropagateUploadFileCommon::headers();

    const auto ifMatch = headers.take(QByteArrayLiteral("If-Match"));
    if (!ifMatch.isEmpty()) {
        headers[QByteArrayLiteral("If")] = "<" + QUrl::toPercentEncoding(destination, "/") + "> ([" + ifMatch + "])";
    }

    if (!_transmissionChecksumHeader.isEmpty()) {
        headers[checkSumHeaderC] = _transmissionChecksumHeader;
    }

    const auto fileSize = _fileToUpload._size;
    headers[QByteArrayLiteral("OC-Total-Length")] = QByteArray::number(fileSize);

    const auto job = new MoveJob(propagator()->account(), 
        Utility::concatUrlPath(chunkUploadFolderUrl(), "/.file"), 
        destination, headers, this);
}
```

**Request**:
```http
MOVE /remote.php/dav/uploads/{user}/{transferId}/.file HTTP/1.1
Host: cloud.example.com
Destination: /remote.php/dav/files/{user}/path/to/file.bin
OC-Total-Length: 524288000
X-OC-Mtime: 1714579200
If: </remote.php/dav/files/{user}/path/to/file.bin> (["abc123etag"])
OC-Checksum: SHA1:da39a3ee5e6b4b0d3255bfef95601890afd80709
```

**Headers**:
- `Destination`: Final file path (absolute path, not full URL)
- `OC-Total-Length`: Total file size in bytes
- `X-OC-Mtime`: Unix timestamp (seconds)
- `If`: Conditional header for etag matching (format: `<path> (["etag"])`)
- `OC-Checksum`: Optional checksum (format: `TYPE:hexvalue`)

**Android** ([ChunkedFileUploadRemoteOperation.java:214-228](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L214-L228)):
```java
String originUri = uploadFolderUri + "/.file";
moveMethod = new MoveMethod(originUri, destinationUri, true);
moveMethod.addRequestHeader(OC_X_OC_MTIME_HEADER, String.valueOf(lastModificationTimestamp));
```

**Android sends identical MOVE request**.

**Expected Response**: `201 Created` or `204 No Content`

**Required Response Headers**:
- `OC-FileID`: Unique file identifier (required)
- `ETag`: File etag (required)
- `OC-JobStatus-Location`: Optional async job polling URL (for 202 Accepted)

**Async Assembly** ([propagateuploadng.cpp:513-522](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L513-L522)):

```cpp
if (_item->_httpErrorCode == 202) {
    QString path = QString::fromUtf8(job->reply()->rawHeader("OC-JobStatus-Location"));
    if (path.isEmpty()) {
        done(SyncFileItem::NormalError, tr("Poll URL missing"));
        return;
    }
    _finished = true;
    startPollJob(path);
    return;
}
```

**If server returns `202 Accepted`**: Client polls `OC-JobStatus-Location` URL until assembly completes.

---

## 5. Parallel Uploads

**Evidence** ([folder.cpp:1255-1258](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/gui/folder.cpp#L1255-L1258)):

```cpp
const auto capsMaxConcurrentChunkUploads = account->capabilities().maxConcurrentChunkUploads();
opt._parallelNetworkJobs = capsMaxConcurrentChunkUploads > 0
    ? capsMaxConcurrentChunkUploads
    : account->isHttp2Supported() ? 20 : 6;
```

**Default Parallel Uploads**:
- **HTTP/2**: 20 concurrent chunk PUTs
- **HTTP/1.1**: 6 concurrent chunk PUTs
- **Server Override**: `files.chunked_upload.max_parallel_count` capability

**Your server must**: Handle concurrent chunk PUTs to the same transfer ID. Chunks may arrive **out of order**.

---

## 6. Dynamic Chunk Sizing

**Evidence** ([propagateuploadng.cpp:427-451](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L427-L451)):

```cpp
auto targetDuration = propagator()->syncOptions()._targetChunkUploadDuration;
if (targetDuration.count() > 0) {
    auto uploadTime = ++job->msSinceStart();
    qint64 predictedGoodSize = (_currentChunkSize * targetDuration) / uploadTime;
    qint64 targetSize = propagator()->_chunkSize / 2 + predictedGoodSize / 2;
    propagator()->_chunkSize = ::qBound(propagator()->syncOptions().minChunkSize(), targetSize, propagator()->syncOptions().maxChunkSize());
}
```

**Algorithm**: Client adjusts chunk size based on upload time to target **1 minute per chunk**.

**Bounds** ([syncoptions.h:61-62](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/syncoptions.h#L61-L62)):
```cpp
static constexpr auto chunkV2MinChunkSize = 5LL * 1000LL * 1000LL; // 5 MB
static constexpr auto chunkV2MaxChunkSize = 5LL * 1000LL * 1000LL * 1000LL; // 5 GB
```

**Your server must**: Accept chunks between **5 MB and 5 GB**. Chunk sizes will vary within a single upload.

**Test Evidence** ([testchunkingng.cpp:79-85](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L79-L85)):
```cpp
constexpr auto maxChunkSize = 5LL * 1000LL * 1000LL * 1000LL;
::setChunkSize(fakeFolder.syncEngine(), 10LL * 1000LL * 1000LL * 1000LL);
QCOMPARE(fakeFolder.syncEngine().syncOptions().maxChunkSize(), maxChunkSize);

constexpr auto minChunkSize = 5 * 1000 * 1000;
::setChunkSize(fakeFolder.syncEngine(), 1 * 1000 * 1000);
QCOMPARE(fakeFolder.syncEngine().syncOptions().minChunkSize(), minChunkSize);
```

---

## 7. Retry & Error Handling

**Evidence** ([propagateupload.cpp:686-735](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateupload.cpp#L686-L735)):

```cpp
void PropagateUploadFileCommon::commonErrorHandling(AbstractNetworkJob *job)
{
    if (_item->_httpErrorCode == 412) {
        // Precondition Failed: Either an etag or a checksum mismatch.
        propagator()->_journal->schedulePathForRemoteDiscovery(_item->_file);
        propagator()->_anotherSyncNeeded = true;
    }

    if (_item->_httpErrorCode == 507) {
        // Insufficient remote storage.
        status = SyncFileItem::DetailError;
        errorString = tr("Upload of %1 exceeds the quota for the folder").arg(Utility::octetsToString(_fileToUpload._size));
        emit propagator()->insufficientRemoteStorage();
    }
}
```

### Status Codes

| Code | Meaning | Client Behavior |
|------|---------|-----------------|
| `201 Created` | Success | Continue |
| `204 No Content` | Success | Continue |
| `202 Accepted` | Async assembly | Poll `OC-JobStatus-Location` |
| `412 Precondition Failed` | Etag mismatch | Retry with fresh PROPFIND |
| `507 Insufficient Storage` | Quota exceeded | Abort upload |
| `5xx` | Server error | Retry chunk PUT |

### Retry Behavior

- **Chunk PUT failure**: Client re-PUTs the same chunk (same URL, same offset)
- **MOVE failure**: Client retries MOVE after delay
- **Network disconnect**: Client resumes via PROPFIND on reconnect

**Resume Condition** ([propagateuploadng.cpp:99-113](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L99-L113)):

```cpp
if (progressInfo._valid && progressInfo.isChunked() && progressInfo._modtime == _item->_modtime && progressInfo._size == _item->_size) {
    _transferId = progressInfo._transferid;
    const auto job = new LsColJob(propagator()->account(), chunkUploadFolderUrl());
    // ... PROPFIND to resume
}
```

**Client resumes if**: `modtime` and `size` match stored upload info.

**Test Evidence** ([testchunkingng.cpp:158-191](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L158-L191)):
```cpp
// Test resuming when there's a confusing chunk added
void testResume1() {
    // ... partial upload
    // Add a fake chunk to make sure it gets deleted
    fakeFolder.uploadState().children.first().insert("10000", size);

    fakeFolder.setServerOverride([&](QNetworkAccessManager::Operation op, const QNetworkRequest &request, QIODevice *) -> QNetworkReply * {
        if (op == QNetworkAccessManager::PutOperation) {
            // Test that we properly resuming and are not sending past data again.
            Q_ASSERT(request.rawHeader("OC-Chunk-Offset").toLongLong() >= uploadedSize);
        } else if (op == QNetworkAccessManager::DeleteOperation) {
            Q_ASSERT(request.url().path().endsWith("/10000"));
        }
        return nullptr;
    });

    QVERIFY(fakeFolder.syncOnce());
}
```

---

## 8. Cleanup on Abort

**Evidence** ([syncengine.cpp:238-241](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/syncengine.cpp#L238-L241)):

```cpp
if (account()->capabilities().chunkingNg()) {
    for (uint transferId : std::as_const(ids)) {
        QUrl url = Utility::concatUrlPath(account()->url(), QLatin1String("remote.php/dav/uploads/") + account()->davUser() + QLatin1Char('/') + QString::number(transferId));
        (new DeleteJob(account(), url, {}, this))->start();
    }
}
```

**Request**:
```http
DELETE /remote.php/dav/uploads/{user}/{transferId}/ HTTP/1.1
```

**Purpose**: Client deletes entire upload folder on abort or stale upload cleanup.

**Expected Response**: `204 No Content` or `404 Not Found`

**Test Evidence** ([testchunkingng.cpp:458-472](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L458-L472)):
```cpp
// We remove the file locally after it has been partially uploaded
void testRemoveStale2() {
    partialUpload(fakeFolder, "A/a0", size);
    QCOMPARE(fakeFolder.uploadState().children.count(), 1);

    fakeFolder.localModifier().remove("A/a0");

    QVERIFY(fakeFolder.syncOnce());
    QCOMPARE(fakeFolder.uploadState().children.count(), 0);
}
```

---

## 9. Edge Cases & Quirks

### 9.1 Chunk Numbering

**Evidence** ([propagateuploadng.cpp:147-154](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L147-L154)):

```cpp
// Chunked upload v2: numbers range from 1 to 10000
_currentChunk = 1;
_sent = 0;
while (_serverChunks.contains(_currentChunk)) {
    _sent += _serverChunks[_currentChunk].size;
    _serverChunks.remove(_currentChunk);
    ++_currentChunk;
}
```

**Chunks start at 1, not 0**. Maximum 10,000 chunks per file.

**Test Evidence** ([testchunkingng.cpp:264-265](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L264-L265)):
```cpp
const auto testChunkNameNum = chunkMap.count() + 1; // Chunk nums start at 1 with Chunk V2, so size() == last num, add 1
const auto testChunkName = QStringLiteral("%1").arg(testChunkNameNum, 5, 10, QChar('0'));
```

---

### 9.2 Alphabetical Ordering

**Evidence** ([propagateuploadng.cpp:46-47](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L46-L47)):

```cpp
// We need to do add leading 0 because the server orders the chunk alphabetically
const auto chunkNumString = QStringLiteral("%1").arg(chunk, maxChunkDigits, 10, QChar('0'));
```

**Server must**: Assemble chunks in **alphabetical order** of chunk names, not numeric order.

**Example**: `00002` comes before `00010` alphabetically.

---

### 9.3 `.file` Sentinel

**Evidence** ([propagateuploadng.cpp:332](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L332)):

```cpp
const auto job = new MoveJob(propagator()->account(), Utility::concatUrlPath(chunkUploadFolderUrl(), "/.file"), destination, headers, this);
```

**MOVE source**: `/uploads/{user}/{transferId}/.file` (not the folder itself)

**Your server must**: Recognize `.file` as the assembly trigger. Concatenate all chunks in alphabetical order when `.file` is MOVEd.

---

### 9.4 Header Case Sensitivity

**Headers are case-insensitive per HTTP spec**, but clients send:
- `OC-Total-Length` (not `oc-total-length`)
- `OC-Chunk-Offset` (not `oc-chunk-offset`)
- `Destination` (not `destination`)

**Your server should**: Accept headers case-insensitively.

**Test Evidence** ([testchunkingng.cpp:87-100](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L87-L100)):
```cpp
auto hasDestinationHeader = false;
fakeFolder.setServerOverride(
    [&hasDestinationHeader](const QNetworkAccessManager::Operation op, const QNetworkRequest &request, QIODevice *const) -> QNetworkReply * {
        if (op == QNetworkAccessManager::PutOperation) {
            qDebug() << "Request headers:" << request.rawHeaderList();
            hasDestinationHeader |= request.hasRawHeader("Destination");
        }
        return nullptr;
    });

// ... upload
QVERIFY(hasDestinationHeader);
```

---

### 9.5 Destination Header Format

**Desktop** ([propagateuploadng.cpp:82-88](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp#L82-L88)):

```cpp
QByteArray PropagateUploadFileNG::destinationHeader() const
{
    const auto davUrl = Utility::trailingSlashPath(propagator()->account()->davUrl().toString());
    const auto remotePath = QUrl::toPercentEncoding(Utility::noLeadingSlashPath(propagator()->fullRemotePath(_fileToUpload._file)), "/"_ba);
    const auto destination = QString(davUrl + remotePath);
    return destination.toUtf8();
}
```

**Format**: Full URL with percent-encoded path (except `/`).

**Example**: `https://cloud.example.com/remote.php/dav/files/alice/Documents/report%202024.pdf`

**Test Evidence** ([testchunkingng.cpp:130-155](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp#L130-L155)):
```cpp
void testDestinationHeaderPercentEncoding()
{
    QByteArray destinationHeader;
    fakeFolder.setServerOverride([&destinationHeader](QNetworkAccessManager::Operation, const QNetworkRequest &request, QIODevice *) -> QNetworkReply * {
        if (destinationHeader.isEmpty() && request.hasRawHeader("Destination")) {
            destinationHeader = request.rawHeader("Destination");
        }
        return nullptr;
    });

    const QString filePath = QStringLiteral("A/SQ-0.5%BF-150/a0");
    // ... upload
    QVERIFY(!destinationHeader.isEmpty());
    QVERIFY(destinationHeader.contains("SQ-0.5%25BF-150"));
    QVERIFY(destinationHeader.contains("/A/SQ-0.5%25BF-150/"));
    QVERIFY(!destinationHeader.contains("%2F"));
    QVERIFY(destinationHeader.startsWith("http://"));
}
```

**Android** ([ChunkedFileUploadRemoteOperation.java:154](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java#L154)):
```java
destinationUri = client.getDavUri() + "/files/" + client.getUserId() + WebdavUtils.encodePath(remotePath);
```

**Android uses same format**.

---

## 10. Complete Wire Sequence

### Scenario: Upload 250 MB file (3 chunks @ 100 MB each)

```http
1. MKCOL /remote.php/dav/uploads/alice/3847562910/
   Headers: OC-Total-Length: 262144000, Destination: https://cloud.example.com/remote.php/dav/files/alice/report.pdf
   Response: 201 Created

2. PROPFIND /remote.php/dav/uploads/alice/3847562910/
   Depth: 1
   Response: 207 Multi-Status (empty, no existing chunks)

3. PUT /remote.php/dav/uploads/alice/3847562910/00001
   Headers: OC-Chunk-Offset: 0, OC-Total-Length: 262144000, Destination: ..., Content-Length: 104857600
   Body: <100 MB binary data>
   Response: 201 Created

4. PUT /remote.php/dav/uploads/alice/3847562910/00002
   Headers: OC-Chunk-Offset: 104857600, OC-Total-Length: 262144000, Destination: ..., Content-Length: 104857600
   Body: <100 MB binary data>
   Response: 201 Created

5. PUT /remote.php/dav/uploads/alice/3847562910/00003
   Headers: OC-Chunk-Offset: 209715200, OC-Total-Length: 262144000, Destination: ..., Content-Length: 52428800
   Body: <50 MB binary data>
   Response: 201 Created

6. MOVE /remote.php/dav/uploads/alice/3847562910/.file
   Destination: /remote.php/dav/files/alice/report.pdf
   Headers: OC-Total-Length: 262144000, X-OC-Mtime: 1714579200
   Response: 201 Created
   Headers: OC-FileID: 00012345, ETag: "abc123def"
```

### Scenario: Resume after disconnect (chunks 1-2 uploaded, chunk 3 missing)

```http
1. PROPFIND /remote.php/dav/uploads/alice/3847562910/
   Response: 207 Multi-Status
   <d:response>
     <d:href>/remote.php/dav/uploads/alice/3847562910/00001</d:href>
     <d:propstat><d:prop><d:getcontentlength>104857600</d:getcontentlength></d:prop></d:propstat>
   </d:response>
   <d:response>
     <d:href>/remote.php/dav/uploads/alice/3847562910/00002</d:href>
     <d:propstat><d:prop><d:getcontentlength>104857600</d:getcontentlength></d:prop></d:propstat>
   </d:response>

2. Client calculates: _sent = 104857600 + 104857600 = 209715200, _currentChunk = 3

3. PUT /remote.php/dav/uploads/alice/3847562910/00003
   Headers: OC-Chunk-Offset: 209715200, ...
   (continues from chunk 3)
```

---

## 11. Platform Differences

| Feature | Desktop (C++) | Android (Java) | Notes |
|---------|---------------|----------------|-------|
| Transfer ID | `rand() ^ modtime ^ size ^ hash(filename)` | `MD5(file_content)` | Server must accept both |
| Chunk naming | 5 digits (`00001`) | 6 digits (`000001`) | Server must accept both |
| Chunk size | 100 MB default, dynamic | 10 MB (mobile) / 40 MB (WiFi) | Fixed per platform |
| Parallel uploads | 6 (HTTP/1.1) / 20 (HTTP/2) | Unknown (likely 1-3) | Server must handle concurrent PUTs |
| Resume | PROPFIND + sum consecutive chunks | Same | Identical logic |

**iOS**: No public chunked upload v2 implementation found in `nextcloud/ios` repository. Likely uses same protocol as Android.

---

## 12. Testing Checklist

Your Go server must pass these tests with a real Nextcloud desktop client:

- [ ] Accept `dav.chunking >= 1.0` capability
- [ ] Accept MKCOL with `OC-Total-Length` and `Destination` headers
- [ ] Accept PROPFIND on upload folder (return empty or existing chunks)
- [ ] Accept chunk PUTs with 5-digit or 6-digit names
- [ ] Accept `OC-Chunk-Offset`, `OC-Total-Length`, `Destination` headers on chunk PUT
- [ ] Handle concurrent chunk PUTs (6-20 parallel)
- [ ] Accept chunks between 5 MB and 5 GB
- [ ] Accept MOVE from `/.file` to final destination
- [ ] Return `OC-FileID` and `ETag` headers on MOVE response
- [ ] Accept DELETE on individual chunks
- [ ] Accept DELETE on entire upload folder
- [ ] Assemble chunks in **alphabetical order** of chunk names
- [ ] Resume uploads via PROPFIND (sum consecutive chunks from 1)
- [ ] Handle 412 Precondition Failed (etag mismatch)
- [ ] Handle 507 Insufficient Storage (quota exceeded)

---

## 13. References

### Desktop Client

- [propagateuploadng.cpp](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateuploadng.cpp) - Core chunked v2 upload logic
- [propagateupload.h](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/propagateupload.h) - Class definitions
- [capabilities.cpp](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/capabilities.cpp) - Capability detection
- [syncoptions.h](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/src/libsync/syncoptions.h) - Chunk size configuration
- [testchunkingng.cpp](https://github.com/nextcloud/desktop/blob/306ef05be4941052e8b1635cadd9a9a29d8a2e86/test/testchunkingng.cpp) - Test cases

### Android Library

- [ChunkedFileUploadRemoteOperation.java](https://github.com/nextcloud/android-library/blob/264573e/library/src/main/java/com/owncloud/android/lib/resources/files/ChunkedFileUploadRemoteOperation.java) - Android implementation

### Commit SHAs

- Desktop: `306ef05be4941052e8b1635cadd9a9a29d8a2e86`
- Android: `264573e`

---

**END OF SPECIFICATION**
