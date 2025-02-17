// Copyright (C) 2021 Storj Labs, Inc.
// See LICENSE for copying information.

/**
 * DownloadTXT is used to download some content as a file.
 */
export class Download {
    public static file(content: string, name: string): void {
        const blob = new Blob([content], {type: 'text/plain'});

        if (window.navigator.msSaveBlob) {
            window.navigator.msSaveBlob(blob, name);
        } else {
            const elem = window.document.createElement('a');
            elem.href = window.URL.createObjectURL(blob);
            elem.download = name;
            document.body.appendChild(elem);
            elem.click();
            document.body.removeChild(elem);
        }
    }
}
