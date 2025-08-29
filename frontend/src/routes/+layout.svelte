<script>
	import '../app.css';
	import favicon from '$lib/assets/favicon.svg';
	import version from '../../version.json?raw';
	const versionData = JSON.parse(version);

	let { children } = $props();
</script>

<svelte:head>
	<link rel="icon" href={favicon} />
</svelte:head>

{@render children?.()}

iterative version {versionData.iteration}
