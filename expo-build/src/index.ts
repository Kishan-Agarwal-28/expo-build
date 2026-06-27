import { AwsClient } from 'aws4fetch';

export default {
	async fetch(request, env, ctx): Promise<Response> {
		try {
			const url = new URL(request.url);
			const file = url.searchParams.get('q');

			if (!file) {
				return new Response('No file provided', { status: 400 });
			}
			const aws = new AwsClient({
				accessKeyId: env.AWS_ACCESS_KEY,
				secretAccessKey: env.AWS_SECRET_KEY,
				region: env.AWS_REGION,
			});
			const endpoint = env.AWS_ENDPOINT.replace(/\/$/, '');
			const s3Url = `${endpoint}/${env.S3_BUCKET_NAME}/${file}`;

			const response = await aws.fetch(s3Url, {
				method: 'GET',
			});

			if (!response.ok) {
				return new Response(`S3 Storage Error: ${response.status}`, { status: response.status });
			}

			const contentLength = response.headers.get('Content-Length');

			return new Response(response.body, {
				status: 200,
				headers: {
					'Content-Type': response.headers.get('Content-Type') || 'application/vnd.android.package-archive',
					...(contentLength && { 'Content-Length': contentLength }),
					'Content-Disposition': `attachment; filename="${file.split('/').pop()}"`,
					'Strict-Transport-Security': 'max-age=31536000; includeSubDomains; preload',
					'X-Content-Type-Options': 'nosniff',
					'Access-Control-Allow-Origin': '*',
				},
			});
		} catch (error) {
			return new Response(`Error: ${error instanceof Error ? error.message : error}`, { status: 500 });
		}
	},
} satisfies ExportedHandler<Env>;
