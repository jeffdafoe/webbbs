<?php

namespace App\Controller;

use App\Service\SettingsService;
use Symfony\Bundle\FrameworkBundle\Controller\AbstractController;
use Symfony\Component\HttpFoundation\JsonResponse;
use Symfony\Component\HttpFoundation\Request;
use Symfony\Component\HttpFoundation\Response;
use Symfony\Component\Routing\Attribute\Route;
use Symfony\Component\Security\Http\Attribute\IsGranted;

class SettingsController extends AbstractController
{
    public function __construct(
        private SettingsService $settingsService
    ) {}

    #[Route('/api/settings/public', name: 'api_settings_public', methods: ['GET'])]
    public function publicSettings(): JsonResponse
    {
        $settings = $this->settingsService->getPublicSettings();

        if (isset($settings['bbs_name'])) {
            $settings['status'] = 'ready';
        } else {
            $settings['status'] = 'setup_required';
        }

        return $this->json($settings);
    }

    #[Route('/api/settings', name: 'api_settings_list', methods: ['GET'])]
    #[IsGranted('ROLE_SYSOP')]
    public function list(): JsonResponse
    {
        return $this->json($this->settingsService->getAllSettings());
    }

    #[Route('/api/settings/{key}', name: 'api_settings_update', methods: ['PUT'])]
    #[IsGranted('ROLE_SYSOP')]
    public function update(string $key, Request $request): JsonResponse
    {
        $data = json_decode($request->getContent(), true);
        if (!isset($data['value'])) {
            return $this->json(['error' => 'Missing value field.'], Response::HTTP_BAD_REQUEST);
        }

        $this->settingsService->set($key, $data['value']);

        return $this->json(['key' => $key, 'value' => $data['value']]);
    }
}
